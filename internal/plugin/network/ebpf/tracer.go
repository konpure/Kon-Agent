package ebpf

import (
	"context"
	"errors"
	"fmt"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"github.com/konpure/Kon-Agent/pkg/plugin"
	"golang.org/x/sys/unix"
	"log/slog"
	"net"
	"os"
	"time"
)

// rttBoundsSeconds mirrors the bucket boundaries in network_monitor.c.
// Keep the two in sync: the kernel only stores bucket indexes, the bounds
// themselves live here (single source of truth on the wire).
var rttBoundsSeconds = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5}

// rtt_stats map layout (see network_monitor.c).
const (
	rttBucketOverflow = 10
	rttKeyCount       = 100
	rttKeySum         = 101
)

type Tracer struct {
	config        plugin.PluginConfig
	stop          chan struct{}
	collector     *collector
	interfaceName string
}

type collector struct {
	obj      *ebpf.Collection
	pktCount *ebpf.Map
	rttStats *ebpf.Map
	xdpLink  link.Link
	kpLink   link.Link
}

func New(config plugin.PluginConfig) plugin.Plugin {
	return &Tracer{
		config: config,
		stop:   make(chan struct{}),
	}
}

func init() {
	plugin.Register("ebpf", New)
}

func (t *Tracer) Name() string {
	return "ebpf"
}

func (t *Tracer) Config() plugin.PluginConfig {
	return t.config
}

func (t *Tracer) Stop() error {
	close(t.stop)
	return nil
}

func (t *Tracer) Run(ctx context.Context, out chan<- plugin.Event) error {
	slog.Info("eBPF plugin started")

	// Initialize eBPF collector
	if err := t.initCollector(); err != nil {
		slog.Error("Failed to initialize eBPF collector", "err", err)
		return err
	}
	defer t.closeCollector()

	ticker := time.NewTicker(t.config.Period)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			t.emitPacketDeltas(out)
			t.emitRTTHistogram(out)
		case <-ctx.Done():
			slog.Info("eBPF plugin stopped")
			return nil
		case <-t.stop:
			slog.Info("eBPF plugin stopped")
			return nil
		}
	}
}

// emitPacketDeltas drains the per-interface counters (read-then-clear) and
// emits one Sum-delta event per interface.
func (t *Tracer) emitPacketDeltas(out chan<- plugin.Event) {
	deltas, err := t.readPacketDeltas()
	if err != nil {
		slog.Error("Failed to read packet deltas", "err", err)
		return
	}
	for ifindex, count := range deltas {
		out <- plugin.Event{
			Name:   "network_packets_total",
			Time:   time.Now().UnixNano(),
			Labels: map[string]string{"interface": interfaceName(ifindex)},
			Values: float64(count),
			Scope:  "ebpf",
			Kind:   plugin.KindSumDelta,
		}
	}
}

// emitRTTHistogram drains the RTT histogram (read-then-clear) and emits one
// Histogram event. RTT is host-wide (kprobe), so it carries no interface label.
func (t *Tracer) emitRTTHistogram(out chan<- plugin.Event) {
	hist, err := t.readRTTHistogram()
	if err != nil {
		slog.Error("Failed to read RTT histogram", "err", err)
		return
	}
	if hist == nil || hist.Count == 0 {
		return
	}
	out <- plugin.Event{
		Name:      "tcp_rtt_seconds",
		Time:      time.Now().UnixNano(),
		Labels:    map[string]string{},
		Scope:     "ebpf",
		Kind:      plugin.KindHistogram,
		Histogram: hist,
	}
	slog.Debug("Emitted RTT histogram", "count", hist.Count, "avg_ms", hist.Sum/float64(hist.Count)*1000)
}

func (t *Tracer) getDefaultInterfaceName() string {
	if t.interfaceName != "" {
		return t.interfaceName
	}

	iface, err := getDefaultInterface()
	if err != nil {
		slog.Error("Failed to get default interface", "err", err)
		return "unknown"
	}
	t.interfaceName = iface
	return iface
}

func (t *Tracer) initCollector() error {
	slog.Info("Removing memory lock limit")
	if err := rlimit.RemoveMemlock(); err != nil {
		slog.Error("Failed to remove memory lock limit", "err", err)
	}

	uid := os.Geteuid()
	if uid != 0 {
		slog.Warn("eBPF plugin is not running as root")
	}

	slog.Info("Loading eBPF program", "path", "internal/plugin/network/ebpf/output/network_monitor.o")
	spec, err := ebpf.LoadCollectionSpec("internal/plugin/network/ebpf/output/network_monitor.o")
	if err != nil {
		return fmt.Errorf("failed to load eBPF spec: %w", err)
	}

	slog.Info("Creating eBPF collection")
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		return fmt.Errorf("failed to create eBPF collection: %w", err)
	}

	pktCount := coll.Maps["pkt_count"]
	if pktCount == nil {
		coll.Close()
		return fmt.Errorf("failed to find pkt_count map")
	}

	rttStats := coll.Maps["rtt_stats"]
	if rttStats == nil {
		coll.Close()
		return fmt.Errorf("failed to find rtt_stats map")
	}

	interfaceName, err := getDefaultInterface()
	t.interfaceName = interfaceName
	if err != nil {
		coll.Close()
		return fmt.Errorf("failed to get default interface: %w", err)
	}
	slog.Info("Using network interface", "interface", interfaceName)

	iface, err := net.InterfaceByName(interfaceName)
	if err != nil {
		coll.Close()
		return fmt.Errorf("failed to get interface by name %s: %w", interfaceName, err)
	}

	slog.Info("Attaching XDP program", "interface", interfaceName, "index", iface.Index)
	xdpLink, err := link.AttachXDP(link.XDPOptions{
		Program:   coll.Programs["track_packets"],
		Interface: iface.Index,
		Flags:     unix.XDP_FLAGS_SKB_MODE,
	})
	if err != nil {
		coll.Close()
		return fmt.Errorf("failed to attach XDP program: %w", err)
	}

	slog.Info("Attaching kprobe", "symbol", "tcp_rcv_established")
	kpLink, err := link.Kprobe("tcp_rcv_established", coll.Programs["tcp_rtt"], nil)
	if err != nil {
		_ = xdpLink.Close()
		coll.Close()
		return fmt.Errorf("failed to attach kprobe: %w", err)
	}

	t.collector = &collector{
		obj:      coll,
		pktCount: pktCount,
		rttStats: rttStats,
		xdpLink:  xdpLink,
		kpLink:   kpLink,
	}
	slog.Info("eBPF collector initialized successfully")
	return nil
}

func (t *Tracer) closeCollector() {
	if t.collector != nil {
		if t.collector.kpLink != nil {
			_ = t.collector.kpLink.Close()
		}
		if t.collector.xdpLink != nil {
			_ = t.collector.xdpLink.Close()
		}
		if t.collector.obj != nil {
			t.collector.obj.Close()
		}
		slog.Info("eBPF collector closed")
	}
}

// readPacketDeltas drains the per-ifindex packet counters (read-then-clear),
// so each reported value is the delta of the last collection window.
func (t *Tracer) readPacketDeltas() (map[uint32]uint64, error) {
	result := make(map[uint32]uint64)
	err := t.drainMap(t.collectorPktCount(), func(key uint32, value uint64) {
		result[key] = value
	})
	return result, err
}

// readRTTHistogram drains the RTT bucket counters (read-then-clear) and
// builds the aggregated histogram. Returns nil when no samples arrived in
// this window.
func (t *Tracer) readRTTHistogram() (*plugin.HistogramData, error) {
	buckets := make([]uint64, len(rttBoundsSeconds)+1)
	var count, sumUS uint64

	err := t.drainMap(t.collectorRTTStats(), func(key uint32, value uint64) {
		switch {
		case key <= rttBucketOverflow:
			buckets[key] = value
		case key == rttKeyCount:
			count = value
		case key == rttKeySum:
			sumUS = value
		}
	})
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}

	return &plugin.HistogramData{
		Count:          count,
		Sum:            float64(sumUS) / 1e6, // microseconds -> seconds
		BucketCounts:   buckets,
		ExplicitBounds: rttBoundsSeconds,
	}, nil
}

// drainMap collects a map's keys, then LookupAndDelete's each of them
// (two phases to avoid mutating the map during iteration).
func (t *Tracer) drainMap(m *ebpf.Map, fn func(key uint32, value uint64)) error {
	if m == nil {
		return fmt.Errorf("collector not initialized")
	}
	var resultErr error

	func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("Recovered from panic", "err", r)
				resultErr = fmt.Errorf("recovered from panic: %v", r)
			}
		}()

		var key uint32
		var value uint64
		var keys []uint32
		iter := m.Iterate()
		for iter.Next(&key, &value) {
			keys = append(keys, key)
		}
		if err := iter.Err(); err != nil {
			resultErr = fmt.Errorf("failed to iterate map: %w", err)
			return
		}

		for _, k := range keys {
			var v uint64
			if err := m.LookupAndDelete(&k, &v); err != nil {
				if errors.Is(err, ebpf.ErrKeyNotExist) {
					continue
				}
				resultErr = fmt.Errorf("failed to lookup and delete: %w", err)
				return
			}
			fn(k, v)
		}
	}()

	return resultErr
}

func (t *Tracer) collectorPktCount() *ebpf.Map {
	if t.collector == nil {
		return nil
	}
	return t.collector.pktCount
}

func (t *Tracer) collectorRTTStats() *ebpf.Map {
	if t.collector == nil {
		return nil
	}
	return t.collector.rttStats
}

// interfaceName resolves an ifindex to its name, falling back to "if<N>".
func interfaceName(ifindex uint32) string {
	if iface, err := net.InterfaceByIndex(int(ifindex)); err == nil {
		return iface.Name
	}
	return fmt.Sprintf("if%d", ifindex)
}

// Get default interface name
func getDefaultInterface() (string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("failed to get network interfaces: %w", err)
	}

	for _, iface := range interfaces {
		// Skip loopback interfaces
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		// Check if interface is up
		if iface.Flags&net.FlagUp != 0 {
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}

			// If interface has IP addresses, consider it active
			if len(addrs) > 0 {
				slog.Info("Found active interface", "interface", iface.Name)
				return iface.Name, nil
			}
		}
	}
	return "", fmt.Errorf("no active network interface found")
}
