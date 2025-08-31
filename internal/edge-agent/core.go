package edge_agent

import (
	"context"
	"fmt"
	"github.com/konpure/Kon-Agent/internal/config"
	"github.com/konpure/Kon-Agent/internal/plugin"
	"github.com/konpure/Kon-Agent/internal/security"
	"github.com/konpure/Kon-Agent/internal/transport/buffer"
	"github.com/konpure/Kon-Agent/internal/transport/quic"
	"github.com/konpure/Kon-Agent/pkg/protocol"
	"log/slog"
	"math"
	"math/rand"
	"os"
	"os/signal"
	"syscall"
	"time"
)

type Core struct {
	cfg             *config.Config
	plugins         *plugin.Manager
	client          *quic.Client
	resourceManager *ResourceManager
	StateManager    *StateManager
	bufferManager   *buffer.Manager
}

func New(cfg *config.Config) *Core {
	pluginManager := plugin.NewManager(cfg)
	client := quic.NewClient(cfg.Server)

	// Init ResourceManager
	resourceManager := NewResourceManager(10*time.Second, 80.0)

	// Init StateManager
	stateManager := NewStateManager(cfg.Cache.Path + string(os.PathSeparator) + "agent_state.json")

	bufferManager := buffer.NewManager()
	return &Core{
		cfg:             cfg,
		plugins:         pluginManager,
		client:          client,
		resourceManager: resourceManager,
		StateManager:    stateManager,
		bufferManager:   bufferManager,
	}
}

func (c *Core) Run() error {
	slog.Info("Agent started", "config", c.cfg)

	// Start StateManager&ResourceManager
	c.StateManager.Start()
	c.resourceManager.Start()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.client.Connect(ctx); err != nil {
		slog.Error("Failed to connect to QUIC server", "error", err)
		c.StateManager.RecordError("connection_failed")
	} else {
		c.StateManager.UpdateConnectionState(true)
	}

	// Setup signal handling for shutdown
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()

	c.plugins.Start(ctx)

	if err := security.DropPrivileges(); err != nil {
		slog.Warn("Failed to drop privileges, continuing with caution", "error", err)
	} else {
		slog.Info("Successfully dropped privileges to minimal set")
	}

	go func(ctx context.Context) {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				buf, err := c.bufferManager.GetOrCreateBuffer("default", 1024)
				if err != nil {
					slog.Error("Failed to get buffer", "error", err)
					continue
				}
				metricsCount := buf.Len()
				if metricsCount == 0 {
					continue
				}
				slog.Info("Flushing metrics from buffer", "count", metricsCount)

				batchSize := 100
				metrics, err := c.bufferManager.GetBatch("default", batchSize)
				if err != nil {
					slog.Error("Failed to get metrics batch", "error", err)
					continue
				}
				if len(metrics) == 0 {
					slog.Debug("No metrics to send in batch")
					continue
				}

				if err := c.sendMetricWithRetry(ctx, metrics, 3); err != nil {
					slog.Error("Failed to send metrics batch", "error", err)
					if putErr := c.bufferManager.PutBatch("default", metrics); putErr != nil {
						slog.Error("Failed to re-insert failed metrics", "error", putErr)
					}
				} else {
					slog.Info("Successfully sent metrics batch", "count", len(metrics))
				}
			case <-ctx.Done():
				slog.Info("Shutting down metric flush goroutine")
				return
			}
		}
	}(ctx)

	go func(ctx context.Context) {
		for {
			select {
			case event, ok := <-c.plugins.Events():
				if !ok {
					return // Channel closed
				}
				slog.Info("Received event",
					"event", event.Name,
					"value", event.Values,
					"labels", event.Labels)

				metric := &protocol.Metric{
					Timestamp: event.Time,
					Name:      event.Name,
					Value:     event.Values,
					Labels:    event.Labels,
				}

				if err := c.bufferManager.PutMetric("default", metric); err != nil {
					slog.Error("Failed to put metric into buffer", "error", err)
					c.StateManager.RecordError("buffer_put_failed")
				}

			case <-ctx.Done():
				slog.Info("Event handler stopped")
				return
			}
		}
	}(ctx)

	go func() {
		for statusChange := range c.plugins.PluginStatusChanges() {
			slog.Info("Plugin status changed", "plugin", statusChange.Name, "status", statusChange.Status, "time", statusChange.Time.Format(time.RFC3339))
			c.StateManager.UpdatePluginStatus(statusChange.Name, string(statusChange.Status))
		}
	}()

	go func(ctx context.Context) {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				for _, name := range c.plugins.GetPluginNames() {
					status := c.plugins.GetPluginStatus(name)
					c.StateManager.UpdatePluginStatus(name, string(status))
				}
			case <-ctx.Done():
				return
			}
		}
	}(ctx)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Wait for interruption signal
	<-sigChan
	slog.Info("Shutting down agent...")

	if err := c.client.Close(); err != nil {
		slog.Error("Failed to close QUIC connection", "error", err)
	}

	c.plugins.Stop()
	c.resourceManager.Stop()
	c.StateManager.Stop()

	slog.Info("Agent stopped")
	return nil
}

func (c *Core) sendMetricWithRetry(ctx context.Context, metrics []*protocol.Metric, maxRetries int) error {
	if len(metrics) == 0 {
		return nil
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			sleepDuration := time.Duration(math.Pow(2, float64(attempt))) * 100 * time.Millisecond
			jitter := time.Duration(rand.Int63n(int64(sleepDuration)))
			sleepDuration += jitter

			select {
			case <-ctx.Done():
				return fmt.Errorf("context cancelled while retrying: %w", ctx.Err())
			case <-time.After(sleepDuration):
			}
		}

		if !c.client.IsConnected() {
			if err := c.client.Connect(ctx); err != nil {
				lastErr = fmt.Errorf("Failed to connect to QUIC server: %w", err)
				continue
			}
		}

		req := &protocol.BatchMetricsRequest{
			Metrics:   metrics,
			AgentId:   c.cfg.ClientId,
			Timestamp: time.Now().UnixNano(),
		}

		if err := c.client.SendBatchMetrics(ctx, req); err != nil {
			lastErr = fmt.Errorf("Failed to send batch metric: %w", err)
			continue
		}
		return nil
	}
	return fmt.Errorf("Failed to send metric after %d attempts: %w", maxRetries, lastErr)
}
