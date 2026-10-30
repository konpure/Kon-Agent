// Package ebpfsched measures CPU runqueue wait latency (golden signal:
// saturation) via sched tracepoints, following bcc's runqlat pattern.
package ebpfsched

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"github.com/konpure/Kon-Agent/internal/plugin/ebpfutil"
	"github.com/konpure/Kon-Agent/pkg/plugin"
)

// runqBoundsSeconds mirrors the log2 buckets in bpf/sched_lat.c
// (1us .. ~1s, 21 bounds -> 22 buckets).
var runqBoundsSeconds = func() []float64 {
	b := make([]float64, 0, 21)
	for i := 0; i < 21; i++ {
		b = append(b, float64(uint64(1)<<i)/1e6)
	}
	return b
}()

// runq_wait map key layout (see bpf/sched_lat.c).
const (
	runqNumBuckets  = 22
	runqKeyCount    = 100
	runqKeySum      = 101
	runqKeySwitches = 102
)

type Tracer struct {
	config    plugin.PluginConfig
	stop      chan struct{}
	collector *collector
}

type collector struct {
	obj       *ebpf.Collection
	runqWait  *ebpf.Map
	wakeupLn  link.Link
	wakeupNew link.Link
	switchLn  link.Link
}

func New(config plugin.PluginConfig) plugin.Plugin {
	return &Tracer{
		config: config,
		stop:   make(chan struct{}),
	}
}

func init() {
	plugin.Register("ebpf_sched", New)
}

func (t *Tracer) Name() string {
	return "ebpf_sched"
}

func (t *Tracer) Config() plugin.PluginConfig {
	return t.config
}

func (t *Tracer) Stop() error {
	close(t.stop)
	return nil
}

func (t *Tracer) Run(ctx context.Context, out chan<- plugin.Event) error {
	slog.Info("eBPF sched plugin started")

	if err := t.initCollector(); err != nil {
		slog.Error("Failed to initialize sched collector", "err", err)
		return err
	}
	defer t.closeCollector()

	ticker := time.NewTicker(t.config.Period)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			t.emitRunqWait(out)
		case <-ctx.Done():
			slog.Info("eBPF sched plugin stopped")
			return nil
		case <-t.stop:
			slog.Info("eBPF sched plugin stopped")
			return nil
		}
	}
}

// emitRunqWait drains the per-CPU histogram (read-then-clear) and emits one
// Histogram event plus the context-switch counter.
func (t *Tracer) emitRunqWait(out chan<- plugin.Event) {
	if t.collector == nil {
		return
	}
	buckets := make([]uint64, runqNumBuckets)
	var count, sumUS, switches uint64

	err := ebpfutil.DrainPercpuU64(t.collector.runqWait, func(key uint32, value uint64) {
		switch {
		case key < runqNumBuckets:
			buckets[key] = value
		case key == runqKeyCount:
			count = value
		case key == runqKeySum:
			sumUS = value
		case key == runqKeySwitches:
			switches = value
		}
	})
	if err != nil {
		slog.Error("Failed to read runqueue wait map", "err", err)
		return
	}

	now := time.Now().UnixNano()
	if switches > 0 {
		out <- plugin.Event{
			Name:   "sched_switches_total",
			Time:   now,
			Labels: map[string]string{},
			Values: float64(switches),
			Scope:  "ebpf_sched",
			Kind:   plugin.KindSumDelta,
		}
	}
	if count == 0 {
		return
	}
	out <- plugin.Event{
		Name:   "cpu_runqueue_wait_seconds",
		Time:   now,
		Labels: map[string]string{},
		Scope:  "ebpf_sched",
		Kind:   plugin.KindHistogram,
		Histogram: &plugin.HistogramData{
			Count:          count,
			Sum:            float64(sumUS) / 1e6, // us -> s
			BucketCounts:   buckets,
			ExplicitBounds: runqBoundsSeconds,
		},
	}
}

func (t *Tracer) initCollector() error {
	if err := rlimit.RemoveMemlock(); err != nil {
		slog.Error("Failed to remove memory lock limit", "err", err)
	}
	if os.Geteuid() != 0 {
		slog.Warn("eBPF sched plugin is not running as root")
	}

	spec, err := ebpf.LoadCollectionSpec("internal/plugin/sched/ebpf/output/sched_lat.o")
	if err != nil {
		return fmt.Errorf("failed to load eBPF spec: %w", err)
	}

	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		return fmt.Errorf("failed to create eBPF collection: %w", err)
	}

	runqWait := coll.Maps["runq_wait"]
	if runqWait == nil {
		coll.Close()
		return fmt.Errorf("failed to find runq_wait map")
	}

	attach := func(group, tpName, progName string) (link.Link, error) {
		prog := coll.Programs[progName]
		if prog == nil {
			return nil, fmt.Errorf("missing program %s", progName)
		}
		l, err := link.Tracepoint(group, tpName, prog, nil)
		if err != nil {
			return nil, fmt.Errorf("attach %s:%s: %w", group, tpName, err)
		}
		return l, nil
	}

	wakeupLn, err := attach("sched", "sched_wakeup", "on_wakeup")
	if err != nil {
		coll.Close()
		return err
	}
	wakeupNewLn, err := attach("sched", "sched_wakeup_new", "on_wakeup_new")
	if err != nil {
		_ = wakeupLn.Close()
		coll.Close()
		return err
	}
	switchLn, err := attach("sched", "sched_switch", "on_switch")
	if err != nil {
		_ = wakeupLn.Close()
		_ = wakeupNewLn.Close()
		coll.Close()
		return err
	}

	t.collector = &collector{
		obj:       coll,
		runqWait:  runqWait,
		wakeupLn:  wakeupLn,
		wakeupNew: wakeupNewLn,
		switchLn:  switchLn,
	}
	slog.Info("eBPF sched collector initialized successfully")
	return nil
}

func (t *Tracer) closeCollector() {
	if t.collector != nil {
		if t.collector.wakeupLn != nil {
			_ = t.collector.wakeupLn.Close()
		}
		if t.collector.wakeupNew != nil {
			_ = t.collector.wakeupNew.Close()
		}
		if t.collector.switchLn != nil {
			_ = t.collector.switchLn.Close()
		}
		if t.collector.obj != nil {
			t.collector.obj.Close()
		}
		slog.Info("eBPF sched collector closed")
	}
}
