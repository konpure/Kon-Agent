package disk

import (
	"context"
	"github.com/konpure/Kon-Agent/pkg/plugin"
	"github.com/shirou/gopsutil/v3/disk"
	"log/slog"
	"time"
)

type DiskPlugin struct {
	config plugin.PluginConfig
	stop   chan struct{}
}

func New(cfg plugin.PluginConfig) plugin.Plugin {
	return &DiskPlugin{
		config: cfg,
		stop:   make(chan struct{}),
	}
}

func init() {
	plugin.Register("disk", New)
}

func (d *DiskPlugin) Name() string {
	return "disk"
}

func (d *DiskPlugin) Config() plugin.PluginConfig {
	return d.config
}

func (d *DiskPlugin) Stop() error {
	close(d.stop)
	return nil
}

func (d *DiskPlugin) Run(ctx context.Context, out chan<- plugin.Event) error {
	slog.Info("Disk plugin started", "period", d.config.Period)

	ticker := time.NewTicker(d.config.Period)
	defer ticker.Stop()

	// Track previous IO stats for delta calculation (map by device name)
	prevIOStats := make(map[string]*disk.IOCountersStat)

	for {
		select {
		case <-ticker.C:
			// Collect disk usage stats
			partitions, err := disk.Partitions(false)
			if err != nil {
				slog.Error("Failed to get disk partitions", "error", err)
				continue
			}

			for _, partition := range partitions {
				// Get usage stats for each partition
				usage, err := disk.Usage(partition.Mountpoint)
				if err != nil {
					slog.Debug("Failed to get disk usage", "mountpoint", partition.Mountpoint, "error", err)
					continue
				}

				// Create labels from partition and usage info
				labels := map[string]string{
					"device":     partition.Device,
					"mountpoint": partition.Mountpoint,
					"fstype":     partition.Fstype,
				}

				// Send disk usage percentage
				out <- plugin.Event{
					Name:   "disk_usage_percent",
					Time:   time.Now().UnixNano(),
					Labels: labels,
					Values: usage.UsedPercent,
				}

				// Send disk usage bytes
				out <- plugin.Event{
					Name:   "disk_usage_bytes",
					Time:   time.Now().UnixNano(),
					Labels: labels,
					Values: float64(usage.Used),
				}

				// Send disk total bytes
				out <- plugin.Event{
					Name:   "disk_total_bytes",
					Time:   time.Now().UnixNano(),
					Labels: labels,
					Values: float64(usage.Total),
				}

				// Send disk free bytes
				out <- plugin.Event{
					Name:   "disk_free_bytes",
					Time:   time.Now().UnixNano(),
					Labels: labels,
					Values: float64(usage.Free),
				}
			}

			// Collect disk I/O stats
			ioStats, err := disk.IOCounters()
			if err != nil {
				slog.Error("Failed to get disk IO stats", "error", err)
				continue
			}

			for _, stat := range ioStats {
				labels := map[string]string{
					"device": stat.Name,
				}

				// Send current IO counters (cumulative)
				out <- plugin.Event{
					Name:   "disk_io_read_bytes_total",
					Time:   time.Now().UnixNano(),
					Labels: labels,
					Values: float64(stat.ReadBytes),
				}

				out <- plugin.Event{
					Name:   "disk_io_write_bytes_total",
					Time:   time.Now().UnixNano(),
					Labels: labels,
					Values: float64(stat.WriteBytes),
				}

				out <- plugin.Event{
					Name:   "disk_io_read_count_total",
					Time:   time.Now().UnixNano(),
					Labels: labels,
					Values: float64(stat.ReadCount),
				}

				out <- plugin.Event{
					Name:   "disk_io_write_count_total",
					Time:   time.Now().UnixNano(),
					Labels: labels,
					Values: float64(stat.WriteCount),
				}

				// Calculate and send IO deltas if we have previous stats
				if prevStat, exists := prevIOStats[stat.Name]; exists {
					readBytesDelta := stat.ReadBytes - prevStat.ReadBytes
					writeBytesDelta := stat.WriteBytes - prevStat.WriteBytes
					readCountDelta := stat.ReadCount - prevStat.ReadCount
					writeCountDelta := stat.WriteCount - prevStat.WriteCount

					// Only send if positive (counters may reset)
					if readBytesDelta >= 0 {
						out <- plugin.Event{
							Name:   "disk_io_read_bytes_delta",
							Time:   time.Now().UnixNano(),
							Labels: labels,
							Values: float64(readBytesDelta),
						}
					}

					if writeBytesDelta >= 0 {
						out <- plugin.Event{
							Name:   "disk_io_write_bytes_delta",
							Time:   time.Now().UnixNano(),
							Labels: labels,
							Values: float64(writeBytesDelta),
						}
					}

					if readCountDelta >= 0 {
						out <- plugin.Event{
							Name:   "disk_io_read_count_delta",
							Time:   time.Now().UnixNano(),
							Labels: labels,
							Values: float64(readCountDelta),
						}
					}

					if writeCountDelta >= 0 {
						out <- plugin.Event{
							Name:   "disk_io_write_count_delta",
							Time:   time.Now().UnixNano(),
							Labels: labels,
							Values: float64(writeCountDelta),
						}
					}
				}

				// Store current stats for next delta calculation
				// Make a copy to avoid reference issues
				statCopy := stat
				prevIOStats[stat.Name] = &statCopy
			}

			slog.Debug("Disk stats collected")

		case <-d.stop:
			slog.Info("Disk plugin stopped by Stop()")
			return nil

		case <-ctx.Done():
			slog.Info("Disk plugin stopped by context")
			return nil
		}
	}
}
