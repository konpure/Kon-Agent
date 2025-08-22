package edge_agent

import (
	"context"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"
	"log/slog"
	"time"
)

type ResourceManager struct {
	ctx       context.Context
	cancel    context.CancelFunc
	interval  time.Duration
	threshold float64
}

func NewResourceManager(interval time.Duration, threshold float64) *ResourceManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &ResourceManager{
		ctx:       ctx,
		cancel:    cancel,
		interval:  interval,
		threshold: threshold,
	}
}

func (rm *ResourceManager) Start() {
	slog.Info("Starting resource manager", "interval", rm.interval, "threshold", rm.threshold)

	go func() {
		ticker := time.NewTicker(rm.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if err := rm.checkResource(); err != nil {
					slog.Error("Failed to check resource", "error", err)
				}
			case <-rm.ctx.Done():
				slog.Info("Resource manager stopped")
				return
			}
		}
	}()
}

func (rm *ResourceManager) checkResource() error {
	// TODO: Implement resource checking logic here
	cpuUsage, err := cpu.Percent(0, false)
	if err != nil {
		return err
	}

	memInfo, err := mem.VirtualMemory()
	if err != nil {
		return err
	}

	slog.Debug("Resourece usage",
		"cpu_usage", cpuUsage[0],
		"mem_usage", memInfo.UsedPercent,
	)

	if cpuUsage[0] > rm.threshold {
		slog.Warn("High CPU usage detected", "cpu_usage", cpuUsage[0], "threshold", rm.threshold)
	}

	if memInfo.UsedPercent > rm.threshold {
		slog.Warn("High memory usage detected", "mem_usage", memInfo.UsedPercent, "threshold", rm.threshold)
	}

	return nil
}
