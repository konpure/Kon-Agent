package ebpf

import (
	"context"
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

type Tracer struct {
	config        plugin.PluginConfig
	stop          chan struct{}
	collector     *collector
	interfaceName string
}

type collector struct {
	obj      *ebpf.Collection
	pktCount *ebpf.Map
	xdpLink  link.Link
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
			slog.Info("Reading packet count")
			count, err := t.readPacketCount()
			if err != nil {
				slog.Error("Failed to read packet count", "err", err)
				continue
			}
			slog.Info("Successfully read packet count", "count", count)

			out <- plugin.Event{
				Name:   "network_packets_total",
				Time:   time.Now().UnixNano(),
				Labels: map[string]string{"interface": t.getDefaultInterfaceName()},
				Values: float64(count),
			}
			slog.Info("Successfully sent packet count event")
		case <-ctx.Done():
			slog.Info("eBPF plugin stopped")
			return nil
		case <-t.stop:
			slog.Info("eBPF plugin stopped")
			return nil
		}
	}
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

	t.collector = &collector{
		obj:      coll,
		pktCount: coll.Maps["pkt_count"],
		xdpLink:  xdpLink,
	}
	slog.Info("eBPF collector initialized successfully")
	return nil
}

func (t *Tracer) closeCollector() {
	if t.collector != nil {
		if t.collector.xdpLink != nil {
			t.collector.xdpLink.Close()
		}
		if t.collector.obj != nil {
			t.collector.obj.Close()
		}
		slog.Info("eBPF collector closed")
	}
}

func (t *Tracer) readPacketCount() (uint64, error) {
	var result uint64
	var resultErr error

	func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("Recovered from panic", "err", r)
				resultErr = fmt.Errorf("recovered from panic: %v", r)
			}
		}()
		if t.collector == nil {
			resultErr = fmt.Errorf("collector not initialized")
			return
		}

		var key uint32 = 0
		var value uint64

		if err := t.collector.pktCount.Lookup(&key, &value); err != nil {
			resultErr = fmt.Errorf("failed to lookup packet count: %w", err)
			return
		}

		result = value
	}()

	return result, resultErr
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
