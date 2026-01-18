package memory

import (
	"context"
	"fmt"
	"github.com/konpure/Kon-Agent/pkg/plugin"
	"github.com/shirou/gopsutil/v3/mem"
	"log/slog"
	"time"
)

type MemoryPlugin struct {
	config plugin.PluginConfig
	stop   chan struct{}
}

func New(cfg plugin.PluginConfig) plugin.Plugin {
	return &MemoryPlugin{
		config: cfg,
		stop:   make(chan struct{}),
	}
}

func init() {
	plugin.Register("memory", New)
}

func (m *MemoryPlugin) Name() string {
	return "memory"
}

func (m *MemoryPlugin) Config() plugin.PluginConfig {
	return m.config
}

func (m *MemoryPlugin) Stop() error {
	close(m.stop)
	return nil
}

func (m *MemoryPlugin) Run(ctx context.Context, out chan<- plugin.Event) error {
	slog.Info("Memory plugin started", "period", m.config.Period)

	ticker := time.NewTicker(m.config.Period)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			stats, err := m.collectMemoryStats()
			if err != nil {
				slog.Error("Failed to collect memory stats", "error", err)
				continue
			}

			// send memory usage percentage
			out <- plugin.Event{
				Name:   "memory_usage_percent",
				Time:   time.Now().UnixNano(),
				Labels: map[string]string{"type": "virtual"},
				Values: stats.VirtualMemory.UsedPercent,
			}

			// send memory usage (bytes)
			out <- plugin.Event{
				Name:   "memory_usage_bytes",
				Time:   time.Now().UnixNano(),
				Labels: map[string]string{"type": "virtual"},
				Values: float64(stats.VirtualMemory.Used),
			}

			// send memory total (bytes)
			out <- plugin.Event{
				Name:   "memory_total_bytes",
				Time:   time.Now().UnixNano(),
				Labels: map[string]string{"type": "virtual"},
				Values: float64(stats.VirtualMemory.Total),
			}

			// send swap memory usage percentage
			if stats.SwapMemory.Total > 0 {
				out <- plugin.Event{
					Name:   "memory_swap_usage_percent",
					Time:   time.Now().UnixNano(),
					Labels: map[string]string{"type": "swap"},
					Values: stats.SwapMemory.UsedPercent,
				}

				out <- plugin.Event{
					Name:   "memory_swap_usage_bytes",
					Time:   time.Now().UnixNano(),
					Labels: map[string]string{"type": "swap"},
					Values: float64(stats.SwapMemory.Used),
				}
			}

			slog.Debug("Memory stats collected",
				"virtual_usage_percent", stats.VirtualMemory.UsedPercent,
				"swap_usage_percent", stats.SwapMemory.UsedPercent)
		case <-m.stop:
			slog.Info("Memory plugin stopped by Stop()")
			return nil

		case <-ctx.Done():
			slog.Info("Memory plugin stopped by context")
			return nil
		}
	}
}

type MemoryStats struct {
	VirtualMemory *mem.VirtualMemoryStat
	SwapMemory    *mem.SwapMemoryStat
}

func (m *MemoryPlugin) collectMemoryStats() (*MemoryStats, error) {
	virtualMem, err := mem.VirtualMemory()
	if err != nil {
		return nil, fmt.Errorf("failed to get virtual memory: %w", err)
	}

	swapMem, err := mem.SwapMemory()
	if err != nil {
		return nil, fmt.Errorf("failed to get swap memory: %w", err)
	}

	return &MemoryStats{
		VirtualMemory: virtualMem,
		SwapMemory:    swapMem,
	}, nil
}
