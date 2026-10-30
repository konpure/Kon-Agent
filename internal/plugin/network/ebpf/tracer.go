package ebpf

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
	"github.com/konpure/Kon-Agent/internal/plugin/ebpfutil"
	"github.com/konpure/Kon-Agent/pkg/plugin"
	"golang.org/x/sys/unix"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
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

// tcp_health map keys (see network_monitor.c).
const (
	tcpHealthRetrans          = 0
	tcpHealthEstablishedDelta = 1
	tcpHealthEstablishedCur   = 2
)

// pkt_size map layout (see network_monitor.c).
const (
	sizeBucketOverflow = 10
	sizeKeyCount       = 100
	sizeKeySum         = 101
)

// pktSizeBoundsBytes mirrors the bucket boundaries in network_monitor.c.
var pktSizeBoundsBytes = []float64{64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384, 32768}

// pktKey/pktVal mirror struct pkt_key/pkt_val in network_monitor.c.
type pktKey struct {
	Ifindex   uint32
	Direction uint8
	L4proto   uint8
	Pad       uint16
}

type pktVal struct {
	Packets uint64
	Bytes   uint64
}

type Tracer struct {
	config        plugin.PluginConfig
	stop          chan struct{}
	collector     *collector
	interfaceName string

	dropMu     sync.Mutex
	dropCounts map[uint32]uint64 // drop reason -> count since last tick
}

// dropEvent mirrors struct drop_event in network_monitor.c.
type dropEvent struct {
	Reason   uint32
	Protocol uint16
	Pad      uint16
	Comm     [16]byte
}

type collector struct {
	obj         *ebpf.Collection
	pktStats    *ebpf.Map
	pktSize     *ebpf.Map
	rttStats    *ebpf.Map
	tcpHealth   *ebpf.Map
	xdpLink     link.Link
	kpLink      link.Link
	egressLink  link.Link
	retransLink link.Link
	stateLink   link.Link
	dropLink    link.Link
	dropRd      *ringbuf.Reader
}

func New(config plugin.PluginConfig) plugin.Plugin {
	return &Tracer{
		config:     config,
		stop:       make(chan struct{}),
		dropCounts: make(map[uint32]uint64),
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

	// Event-driven drop channel: consume the ring buffer in the background,
	// aggregate per reason, and let the periodic tick emit the counts.
	go t.readDrops()

	ticker := time.NewTicker(t.config.Period)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			t.maybeReattachXDP()
			t.emitPacketFlows(out)
			t.emitPacketSizes(out)
			t.emitRTTHistogram(out)
			t.emitTCPHealth(out)
			t.emitDrops(out)
		case <-ctx.Done():
			slog.Info("eBPF plugin stopped")
			return nil
		case <-t.stop:
			slog.Info("eBPF plugin stopped")
			return nil
		}
	}
}

// emitPacketFlows drains the flow stats map (read-then-clear) and emits
// packets and bytes per (interface, direction, protocol) series.
func (t *Tracer) emitPacketFlows(out chan<- plugin.Event) {
	flows, err := t.readFlowStats()
	if err != nil {
		slog.Error("Failed to read flow stats", "err", err)
		return
	}
	now := time.Now().UnixNano()
	for key, val := range flows {
		labels := map[string]string{
			"interface": interfaceName(key.Ifindex),
			"direction": dirName(key.Direction),
			"protocol":  protoName(key.L4proto),
		}
		out <- plugin.Event{
			Name:   "network_packets_total",
			Time:   now,
			Labels: labels,
			Values: float64(val.Packets),
			Scope:  "ebpf",
			Kind:   plugin.KindSumDelta,
		}
		out <- plugin.Event{
			Name:   "network_bytes_total",
			Time:   now,
			Labels: labels,
			Values: float64(val.Bytes),
			Scope:  "ebpf",
			Kind:   plugin.KindSumDelta,
		}
	}
}

// emitPacketSizes drains the packet size histogram (read-then-clear) and
// emits one Histogram event. Ingress only (XDP).
func (t *Tracer) emitPacketSizes(out chan<- plugin.Event) {
	if t.collector == nil {
		return
	}
	buckets := make([]uint64, sizeBucketOverflow+1)
	var count, sumBytes uint64

	// pkt_size is a PERCPU_HASH: sum per-CPU values per key.
	err := ebpfutil.DrainPercpuU64(t.collector.pktSize, func(key uint32, value uint64) {
		switch {
		case key <= sizeBucketOverflow:
			buckets[key] = value
		case key == sizeKeyCount:
			count = value
		case key == sizeKeySum:
			sumBytes = value
		}
	})
	if err != nil {
		slog.Error("Failed to read packet size histogram", "err", err)
		return
	}
	if count == 0 {
		return
	}
	out <- plugin.Event{
		Name:   "network_packet_size_bytes",
		Time:   time.Now().UnixNano(),
		Labels: map[string]string{},
		Scope:  "ebpf",
		Kind:   plugin.KindHistogram,
		Histogram: &plugin.HistogramData{
			Count:          count,
			Sum:            float64(sumBytes),
			BucketCounts:   buckets,
			ExplicitBounds: pktSizeBoundsBytes,
		},
	}
}

// readDrops consumes the drop ring buffer until it is closed.
func (t *Tracer) readDrops() {
	for {
		record, err := t.collector.dropRd.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				return
			}
			slog.Error("Failed to read drop event", "err", err)
			continue
		}
		var ev dropEvent
		if err := binary.Read(bytes.NewReader(record.RawSample), binary.LittleEndian, &ev); err != nil {
			continue
		}
		t.dropMu.Lock()
		t.dropCounts[ev.Reason]++
		t.dropMu.Unlock()
	}
}

// emitDrops flushes the aggregated drop counts as a Sum-delta metric.
func (t *Tracer) emitDrops(out chan<- plugin.Event) {
	t.dropMu.Lock()
	counts := t.dropCounts
	t.dropCounts = make(map[uint32]uint64)
	t.dropMu.Unlock()

	now := time.Now().UnixNano()
	for reason, count := range counts {
		out <- plugin.Event{
			Name:   "packet_drops_total",
			Time:   now,
			Labels: map[string]string{"reason": dropReasonName(reason)},
			Values: float64(count),
			Scope:  "ebpf",
			Kind:   plugin.KindSumDelta,
		}
	}
}

// dropReasonSupported reports whether the kernel's kfree_skb tracepoint
// carries a drop reason field at the fixed offset 28 (kernel 5.17+).
// The offset must match DROP_REASON_OFF in network_monitor.c because the
// verifier requires constant ctx offsets in the BPF program.
func dropReasonSupported() bool {
	for _, base := range []string{"/sys/kernel/tracing", "/sys/kernel/debug/tracing"} {
		data, err := os.ReadFile(base + "/events/skb/kfree_skb/format")
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "field:") || !strings.Contains(line, " reason;") {
				continue
			}
			for _, part := range strings.Split(line, ";") {
				part = strings.TrimSpace(part)
				if !strings.HasPrefix(part, "offset:") {
					continue
				}
				off, err := strconv.ParseUint(strings.TrimPrefix(part, "offset:"), 10, 32)
				return err == nil && off == 28
			}
		}
	}
	return false
}

// dropReasonName maps SKB_DROP_REASON_* values to short labels.
// 0 means "unknown" (kernel < 5.17 or unprobed).
func dropReasonName(reason uint32) string {
	switch reason {
	case 0:
		return "unknown"
	case 1:
		return "unspecified"
	case 2:
		return "no_socket"
	case 3:
		return "pkt_too_small"
	case 4:
		return "tcp_csum"
	case 5:
		return "socket_filter"
	case 6:
		return "ipv6_disabled"
	default:
		return "reason_" + strconv.FormatUint(uint64(reason), 10)
	}
}

func dirName(d uint8) string {
	if d == 1 {
		return "egress"
	}
	return "ingress"
}

// protoName maps the L4 protocol number to a label value. 0 means "all"
// (egress kprobe path, where headers are not parsed).
func protoName(p uint8) string {
	switch p {
	case 0:
		return "all"
	case 1:
		return "icmp"
	case 6:
		return "tcp"
	case 17:
		return "udp"
	case 58:
		return "icmpv6"
	default:
		return "other"
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

// emitTCPHealth emits TCP health metrics: retransmit and new-connection
// counts are deltas (read-then-clear), the established count is a gauge
// (read without clearing).
func (t *Tracer) emitTCPHealth(out chan<- plugin.Event) {
	if t.collector == nil {
		return
	}
	now := time.Now().UnixNano()

	if retrans := t.drainKey(tcpHealthRetrans); retrans > 0 {
		out <- plugin.Event{
			Name:   "tcp_retransmissions_total",
			Time:   now,
			Labels: map[string]string{},
			Values: float64(retrans),
			Scope:  "ebpf",
			Kind:   plugin.KindSumDelta,
		}
	}

	if established := t.drainKey(tcpHealthEstablishedDelta); established > 0 {
		out <- plugin.Event{
			Name:   "tcp_connections_total",
			Time:   now,
			Labels: map[string]string{},
			Values: float64(established),
			Scope:  "ebpf",
			Kind:   plugin.KindSumDelta,
		}
	}

	// Gauge: always report the current value, even zero.
	out <- plugin.Event{
		Name:   "tcp_established",
		Time:   now,
		Labels: map[string]string{},
		Values: float64(t.readKey(tcpHealthEstablishedCur)),
		Scope:  "ebpf",
		Kind:   plugin.KindGauge,
	}
}

// drainKey reads and clears one counter key (delta semantics).
func (t *Tracer) drainKey(key uint32) uint64 {
	var v uint64
	k := key
	if err := t.collector.tcpHealth.LookupAndDelete(&k, &v); err != nil {
		return 0
	}
	return v
}

// readKey reads one counter key without clearing it (gauge semantics).
func (t *Tracer) readKey(key uint32) uint64 {
	var v uint64
	k := key
	if err := t.collector.tcpHealth.Lookup(&k, &v); err != nil {
		return 0
	}
	return v
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

	pktStats := coll.Maps["pkt_stats"]
	if pktStats == nil {
		coll.Close()
		return fmt.Errorf("failed to find pkt_stats map")
	}

	pktSize := coll.Maps["pkt_size"]
	if pktSize == nil {
		coll.Close()
		return fmt.Errorf("failed to find pkt_size map")
	}

	rttStats := coll.Maps["rtt_stats"]
	if rttStats == nil {
		coll.Close()
		return fmt.Errorf("failed to find rtt_stats map")
	}

	tcpHealth := coll.Maps["tcp_health"]
	if tcpHealth == nil {
		coll.Close()
		return fmt.Errorf("failed to find tcp_health map")
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
	xdpLink, err := attachXDPWithFallback(coll.Programs["track_packets"], iface.Index)
	if err != nil {
		coll.Close()
		return fmt.Errorf("failed to attach XDP program: %w", err)
	}

	kpLink, kpMode, err := ebpfutil.TryAttach(coll, ebpfutil.Hook{
		FentryProg: "tcp_rtt_fentry",
		KprobeSym:  "tcp_rcv_established",
		KprobeProg: "tcp_rtt",
	})
	if err != nil {
		_ = xdpLink.Close()
		coll.Close()
		return fmt.Errorf("failed to attach RTT hook: %w", err)
	}
	slog.Info("RTT hook attached", "via", kpMode)

	slog.Info("Attaching egress kprobe", "symbol", "dev_queue_xmit")
	egressLink, err := link.Kprobe("dev_queue_xmit", coll.Programs["dev_egress"], nil)
	if err != nil {
		_ = kpLink.Close()
		_ = xdpLink.Close()
		coll.Close()
		return fmt.Errorf("failed to attach egress kprobe: %w", err)
	}

	slog.Info("Attaching tracepoints", "points", "tcp:tcp_retransmit_skb, sock:inet_sock_set_state")
	retransLink, err := link.Tracepoint("tcp", "tcp_retransmit_skb", coll.Programs["tcp_retrans"], nil)
	if err != nil {
		_ = egressLink.Close()
		_ = kpLink.Close()
		_ = xdpLink.Close()
		coll.Close()
		return fmt.Errorf("failed to attach tcp_retransmit_skb tracepoint: %w", err)
	}
	stateLink, err := link.Tracepoint("sock", "inet_sock_set_state", coll.Programs["tcp_set_state"], nil)
	if err != nil {
		_ = retransLink.Close()
		_ = egressLink.Close()
		_ = kpLink.Close()
		_ = xdpLink.Close()
		coll.Close()
		return fmt.Errorf("failed to attach inet_sock_set_state tracepoint: %w", err)
	}

	// Packet drop channel: probe whether the reason field exists at the
	// fixed offset (5.17+), then attach and open the ring buffer.
	dropCfg := coll.Maps["drop_cfg"]
	if dropReasonSupported() {
		var zero uint32
		var one uint32 = 1
		if err := dropCfg.Update(zero, one, 0); err != nil {
			slog.Warn("Failed to set drop reason flag, reasons will be unknown", "err", err)
		} else {
			slog.Info("Drop reason field probed")
		}
	} else {
		slog.Info("Drop reason field not present on this kernel (< 5.17)")
	}
	dropLink, err := link.Tracepoint("skb", "kfree_skb", coll.Programs["on_kfree_skb"], nil)
	if err != nil {
		_ = stateLink.Close()
		_ = retransLink.Close()
		_ = egressLink.Close()
		_ = kpLink.Close()
		_ = xdpLink.Close()
		coll.Close()
		return fmt.Errorf("failed to attach kfree_skb tracepoint: %w", err)
	}
	dropRd, err := ringbuf.NewReader(coll.Maps["drop_events"])
	if err != nil {
		_ = dropLink.Close()
		_ = stateLink.Close()
		_ = retransLink.Close()
		_ = egressLink.Close()
		_ = kpLink.Close()
		_ = xdpLink.Close()
		coll.Close()
		return fmt.Errorf("failed to open drop ring buffer: %w", err)
	}

	t.collector = &collector{
		obj:         coll,
		pktStats:    pktStats,
		pktSize:     pktSize,
		rttStats:    rttStats,
		tcpHealth:   tcpHealth,
		xdpLink:     xdpLink,
		kpLink:      kpLink,
		egressLink:  egressLink,
		retransLink: retransLink,
		stateLink:   stateLink,
		dropLink:    dropLink,
		dropRd:      dropRd,
	}
	slog.Info("eBPF collector initialized successfully")
	return nil
}

func (t *Tracer) closeCollector() {
	if t.collector != nil {
		if t.collector.dropRd != nil {
			_ = t.collector.dropRd.Close()
		}
		if t.collector.dropLink != nil {
			_ = t.collector.dropLink.Close()
		}
		if t.collector.retransLink != nil {
			_ = t.collector.retransLink.Close()
		}
		if t.collector.stateLink != nil {
			_ = t.collector.stateLink.Close()
		}
		if t.collector.egressLink != nil {
			_ = t.collector.egressLink.Close()
		}
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

// readFlowStats drains the pkt_stats map (read-then-clear). pkt_stats is a
// PERCPU_HASH: each key holds one value per CPU, summed here.
func (t *Tracer) readFlowStats() (map[pktKey]pktVal, error) {
	result := make(map[pktKey]pktVal)
	if t.collector == nil || t.collector.pktStats == nil {
		return result, fmt.Errorf("collector not initialized")
	}

	m := t.collector.pktStats
	var key pktKey
	var vals []pktVal
	var keys []pktKey
	iter := m.Iterate()
	for iter.Next(&key, &vals) {
		var total pktVal
		for _, v := range vals {
			total.Packets += v.Packets
			total.Bytes += v.Bytes
		}
		keys = append(keys, key)
		result[key] = total
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate flow stats map: %w", err)
	}
	for _, k := range keys {
		if err := m.Delete(&k); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			return nil, fmt.Errorf("failed to delete flow key: %w", err)
		}
	}
	return result, nil
}

// readRTTHistogram drains the RTT bucket counters (read-then-clear) and
// builds the aggregated histogram. Returns nil when no samples arrived in
// this window.
func (t *Tracer) readRTTHistogram() (*plugin.HistogramData, error) {
	if t.collector == nil || t.collector.rttStats == nil {
		return nil, fmt.Errorf("collector not initialized")
	}

	buckets := make([]uint64, len(rttBoundsSeconds)+1)
	var count, sumUS uint64

	m := t.collector.rttStats
	var key uint32
	var value uint64
	var keys []uint32
	iter := m.Iterate()
	for iter.Next(&key, &value) {
		keys = append(keys, key)
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate rtt map: %w", err)
	}
	for _, k := range keys {
		var v uint64
		if err := m.LookupAndDelete(&k, &v); err != nil {
			if errors.Is(err, ebpf.ErrKeyNotExist) {
				continue
			}
			return nil, fmt.Errorf("failed to lookup and delete: %w", err)
		}
		switch {
		case k <= rttBucketOverflow:
			buckets[k] = v
		case k == rttKeyCount:
			count = v
		case k == rttKeySum:
			sumUS = v
		}
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

// attachXDPWithFallback tries driver (native) mode first and falls back to
// generic (SKB) mode. Native mode runs the program in the NIC driver before
// skb allocation; generic mode is always available but costs more per packet.
func attachXDPWithFallback(prog *ebpf.Program, ifindex int) (link.Link, error) {
	l, err := link.AttachXDP(link.XDPOptions{
		Program:   prog,
		Interface: ifindex,
		Flags:     unix.XDP_FLAGS_DRV_MODE,
	})
	if err == nil {
		slog.Info("XDP attached in driver (native) mode")
		return l, nil
	}
	slog.Warn("XDP driver mode unavailable, falling back to generic (SKB) mode", "err", err)
	l, err = link.AttachXDP(link.XDPOptions{
		Program:   prog,
		Interface: ifindex,
		Flags:     unix.XDP_FLAGS_SKB_MODE,
	})
	if err != nil {
		return nil, err
	}
	slog.Info("XDP attached in generic (SKB) mode")
	return l, nil
}

// interfaceName resolves an ifindex to its name, falling back to "if<N>".
func interfaceName(ifindex uint32) string {
	if iface, err := net.InterfaceByIndex(int(ifindex)); err == nil {
		return iface.Name
	}
	return fmt.Sprintf("if%d", ifindex)
}

// maybeReattachXDP follows the default route: when the system switches to
// another egress interface (hotplug, route change), re-attach the XDP
// program. The new link is attached before the old one is dropped — a brief
// double-attach only double-counts for a moment; the reverse would lose
// packets.
func (t *Tracer) maybeReattachXDP() {
	name, err := defaultRouteInterface()
	if err != nil || name == "" || name == t.interfaceName {
		return
	}
	iface, err := net.InterfaceByName(name)
	if err != nil || t.collector == nil {
		return
	}
	slog.Info("Default route interface changed, re-attaching XDP",
		"from", t.interfaceName, "to", name)
	newLink, err := attachXDPWithFallback(t.collector.obj.Programs["track_packets"], iface.Index)
	if err != nil {
		slog.Error("Failed to re-attach XDP", "interface", name, "err", err)
		return
	}
	if t.collector.xdpLink != nil {
		_ = t.collector.xdpLink.Close()
	}
	t.collector.xdpLink = newLink
	t.interfaceName = name
}

// defaultRouteInterface returns the interface carrying the default IPv4
// route, parsed from /proc/net/route (no netlink dependency).
func defaultRouteInterface() (string, error) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return "", fmt.Errorf("failed to open /proc/net/route: %w", err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Scan() // header line
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		if fields[1] == "00000000" { // default route
			return fields[0], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("failed to parse /proc/net/route: %w", err)
	}
	return "", fmt.Errorf("no default route found")
}

// Get default interface name: prefer the default-route interface (the first
// active-interface heuristic can pick docker0 or other virtual NICs).
func getDefaultInterface() (string, error) {
	if name, err := defaultRouteInterface(); err == nil {
		slog.Info("Using default route interface", "interface", name)
		return name, nil
	}

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
