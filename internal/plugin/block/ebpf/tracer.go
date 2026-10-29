// Package ebpfblock collects block-device IO latency distribution and
// per-device completion counters via eBPF raw tracepoints.
//
// It follows the same kernel-pre-aggregation pattern as the network plugin:
// the kernel maintains maps, userspace periodically drains them
// (read-then-clear -> delta temporality).
package ebpfblock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"github.com/konpure/Kon-Agent/pkg/plugin"
)

// Bucket bounds in seconds: 2^i microseconds for i in 0..26.
// Keep in sync with the log2_slot buckets in bpf/block_io.c.
var ioLatencyBoundsSeconds = func() []float64 {
	b := make([]float64, 0, 27)
	for i := 0; i < 27; i++ {
		b = append(b, float64(uint64(1)<<i)/1e6)
	}
	return b
}()

// io_latency map key layout (see bpf/block_io.c).
const (
	numBuckets   = 28
	writeShift   = 64
	keyCountBase = 100 // + rw
	keySumBase   = 110 // + rw
)

var rwNames = [2]string{"read", "write"}

type Tracer struct {
	config    plugin.PluginConfig
	stop      chan struct{}
	collector *collector
	devNames  map[uint64]string // devt -> resolved device name cache
}

type collector struct {
	obj         *ebpf.Collection
	ioLatency   *ebpf.Map
	ioCompleted *ebpf.Map
	issueLink   link.Link
	complLink   link.Link
}

func New(config plugin.PluginConfig) plugin.Plugin {
	return &Tracer{
		config:   config,
		stop:     make(chan struct{}),
		devNames: make(map[uint64]string),
	}
}

func init() {
	plugin.Register("ebpf_block", New)
}

func (t *Tracer) Name() string {
	return "ebpf_block"
}

func (t *Tracer) Config() plugin.PluginConfig {
	return t.config
}

func (t *Tracer) Stop() error {
	close(t.stop)
	return nil
}

func (t *Tracer) Run(ctx context.Context, out chan<- plugin.Event) error {
	slog.Info("eBPF block plugin started")

	if err := t.initCollector(); err != nil {
		slog.Error("Failed to initialize block IO collector", "err", err)
		return err
	}
	defer t.closeCollector()

	ticker := time.NewTicker(t.config.Period)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			t.emitLatency(out)
			t.emitCompleted(out)
		case <-ctx.Done():
			slog.Info("eBPF block plugin stopped")
			return nil
		case <-t.stop:
			slog.Info("eBPF block plugin stopped")
			return nil
		}
	}
}

// latencyAcc accumulates one read/write direction of the histogram.
type latencyAcc struct {
	buckets [numBuckets]uint64
	count   uint64
	sumUS   uint64
}

// emitLatency drains the histogram map and emits one Histogram event per
// direction (read/write). Empty windows are skipped.
func (t *Tracer) emitLatency(out chan<- plugin.Event) {
	if t.collector == nil {
		return
	}
	accs := [2]*latencyAcc{{}, {}}

	err := t.drainMap32(t.collector.ioLatency, func(key uint32, value uint64) {
		switch {
		case key < writeShift && key < numBuckets:
			accs[0].buckets[key] += value
		case key >= writeShift && key < writeShift+numBuckets:
			accs[1].buckets[key-writeShift] += value
		case key == keyCountBase:
			accs[0].count = value
		case key == keyCountBase+1:
			accs[1].count = value
		case key == keySumBase:
			accs[0].sumUS = value
		case key == keySumBase+1:
			accs[1].sumUS = value
		}
	})
	if err != nil {
		slog.Error("Failed to read IO latency map", "err", err)
		return
	}

	for rw, acc := range accs {
		if acc.count == 0 {
			continue
		}
		buckets := make([]uint64, numBuckets)
		copy(buckets, acc.buckets[:])
		out <- plugin.Event{
			Name:   "disk_io_latency_seconds",
			Time:   time.Now().UnixNano(),
			Labels: map[string]string{"rw": rwNames[rw]},
			Scope:  "ebpf_block",
			Kind:   plugin.KindHistogram,
			Histogram: &plugin.HistogramData{
				Count:          acc.count,
				Sum:            float64(acc.sumUS) / 1e6, // us -> s
				BucketCounts:   buckets,
				ExplicitBounds: ioLatencyBoundsSeconds,
			},
		}
	}
}

// emitCompleted drains the per-device completion counter and emits one
// Sum-delta event per device.
func (t *Tracer) emitCompleted(out chan<- plugin.Event) {
	if t.collector == nil {
		return
	}
	err := t.drainMap64(t.collector.ioCompleted, func(devt uint64, count uint64) {
		out <- plugin.Event{
			Name:   "disk_io_completed_total",
			Time:   time.Now().UnixNano(),
			Labels: map[string]string{"device": t.deviceName(devt)},
			Values: float64(count),
			Scope:  "ebpf_block",
			Kind:   plugin.KindSumDelta,
		}
	})
	if err != nil {
		slog.Error("Failed to read IO completed map", "err", err)
	}
}

// deviceName resolves devt to a device name via sysfs, with caching.
func (t *Tracer) deviceName(devt uint64) string {
	if name, ok := t.devNames[devt]; ok {
		return name
	}
	major := uint32(devt >> 20)
	minor := uint32(devt & 0xFFFFF)
	name := fmt.Sprintf("%d:%d", major, minor)
	target, err := os.Readlink(fmt.Sprintf("/sys/dev/block/%d:%d", major, minor))
	if err == nil {
		name = filepath.Base(target)
	}
	t.devNames[devt] = name
	return name
}

func (t *Tracer) initCollector() error {
	if err := rlimit.RemoveMemlock(); err != nil {
		slog.Error("Failed to remove memory lock limit", "err", err)
	}
	if os.Geteuid() != 0 {
		slog.Warn("eBPF block plugin is not running as root")
	}

	spec, err := ebpf.LoadCollectionSpec("internal/plugin/block/ebpf/output/block_io.o")
	if err != nil {
		return fmt.Errorf("failed to load eBPF spec: %w", err)
	}

	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		return fmt.Errorf("failed to create eBPF collection: %w", err)
	}

	ioLatency := coll.Maps["io_latency"]
	ioCompleted := coll.Maps["io_completed"]
	if ioLatency == nil || ioCompleted == nil {
		coll.Close()
		return fmt.Errorf("missing io_latency or io_completed map")
	}

	issueLink, err := attachWithFallback(coll, "block_rq_issue", "block_rq_issue", "blk_mq_start_request", "blk_start")
	if err != nil {
		coll.Close()
		return fmt.Errorf("failed to attach issue hook: %w", err)
	}

	complLink, err := attachWithFallback(coll, "block_rq_complete", "block_rq_complete", "blk_mq_end_request", "blk_done")
	if err != nil {
		_ = issueLink.Close()
		coll.Close()
		return fmt.Errorf("failed to attach complete hook: %w", err)
	}

	t.collector = &collector{
		obj:         coll,
		ioLatency:   ioLatency,
		ioCompleted: ioCompleted,
		issueLink:   issueLink,
		complLink:   complLink,
	}
	slog.Info("eBPF block collector initialized successfully")
	return nil
}

// attachWithFallback tries the raw tracepoint first (stable ABI, typed args),
// then falls back to a kprobe on the equivalent function. This is a first
// instance of the planned runtime degradation chain (fentry → raw_tp → kprobe).
func attachWithFallback(coll *ebpf.Collection, tpName, tpProg, kpSym, kpProg string) (link.Link, error) {
	if p := coll.Programs[tpProg]; p != nil {
		l, err := link.AttachRawTracepoint(link.RawTracepointOptions{Name: tpName, Program: p})
		if err == nil {
			slog.Info("Attached raw tracepoint", "name", tpName)
			return l, nil
		}
		slog.Warn("Raw tracepoint attach failed, falling back to kprobe", "name", tpName, "err", err)
	}
	p := coll.Programs[kpProg]
	if p == nil {
		return nil, fmt.Errorf("missing fallback program %s", kpProg)
	}
	l, err := link.Kprobe(kpSym, p, nil)
	if err != nil {
		return nil, fmt.Errorf("kprobe %s: %w", kpSym, err)
	}
	slog.Info("Attached kprobe fallback", "symbol", kpSym)
	return l, nil
}

func (t *Tracer) closeCollector() {
	if t.collector != nil {
		if t.collector.issueLink != nil {
			_ = t.collector.issueLink.Close()
		}
		if t.collector.complLink != nil {
			_ = t.collector.complLink.Close()
		}
		if t.collector.obj != nil {
			t.collector.obj.Close()
		}
		slog.Info("eBPF block collector closed")
	}
}

// drainMap32 collects keys first, then LookupAndDelete each of them
// (two phases to avoid mutating the map during iteration).
func (t *Tracer) drainMap32(m *ebpf.Map, fn func(key uint32, value uint64)) error {
	var key uint32
	var value uint64
	var keys []uint32
	iter := m.Iterate()
	for iter.Next(&key, &value) {
		keys = append(keys, key)
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("failed to iterate map: %w", err)
	}
	for _, k := range keys {
		var v uint64
		if err := m.LookupAndDelete(&k, &v); err != nil {
			if errors.Is(err, ebpf.ErrKeyNotExist) {
				continue
			}
			return fmt.Errorf("failed to lookup and delete: %w", err)
		}
		fn(k, v)
	}
	return nil
}

// drainMap64 is drainMap32 for u64-keyed maps.
func (t *Tracer) drainMap64(m *ebpf.Map, fn func(key uint64, value uint64)) error {
	var key uint64
	var value uint64
	var keys []uint64
	iter := m.Iterate()
	for iter.Next(&key, &value) {
		keys = append(keys, key)
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("failed to iterate map: %w", err)
	}
	for _, k := range keys {
		var v uint64
		if err := m.LookupAndDelete(&k, &v); err != nil {
			if errors.Is(err, ebpf.ErrKeyNotExist) {
				continue
			}
			return fmt.Errorf("failed to lookup and delete: %w", err)
		}
		fn(k, v)
	}
	return nil
}
