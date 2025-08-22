package edge_agent

import (
	"context"
	"fmt"
	"github.com/konpure/Kon-Agent/internal/config"
	"github.com/konpure/Kon-Agent/internal/plugin"
	"github.com/konpure/Kon-Agent/internal/transport/quic"
	"github.com/konpure/Kon-Agent/pkg/protocol"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

type Core struct {
	cfg     *config.Config
	plugins *plugin.Manager
	client  *quic.Client
}

func New(cfg *config.Config) *Core {
	pluginManager := plugin.NewManager(cfg)
	client := quic.NewClient(cfg.Server)

	return &Core{
		cfg:     cfg,
		plugins: pluginManager,
		client:  client,
	}
}

func (c *Core) Run() error {
	slog.Info("Agent started", "config", c.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.client.Connect(ctx); err != nil {
		slog.Error("Failed to connect to QUIC server", "error", err)
	}

	// Setup signal handling for shutdown
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()

	c.plugins.Start(ctx)

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

				if err := c.sendMetricWithRetry(ctx, metric, 3); err != nil {
					slog.Error("Failed to send metric after retries", "error", err)
				}

			case <-ctx.Done():
				slog.Info("Event handler stopped")
			}
		}
	}(ctx)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Wait for interruption signal
	<-sigChan
	slog.Info("Shutting down agent...")

	c.plugins.Stop()

	slog.Info("Agent stopped")
	return nil
}

func (c *Core) sendMetricWithRetry(ctx context.Context, metric *protocol.Metric, maxRetries int) error {
	var lastErr error

	for i := 0; i <= maxRetries; i++ {
		sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)

		err := c.client.SendMetric(sendCtx, metric)
		cancel()

		if err == nil {
			return nil
		}

		lastErr = err
		slog.Warn("Failed to send metric", "attempt", i+1, "err", err)

		if i < maxRetries {
			slog.Info("Attempting to reconnect to QUIC server")

			reconnectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if reconnectErr := c.client.Connect(reconnectCtx); reconnectErr != nil {
				slog.Error("Failed to reconnect to QUIC server", "err", reconnectErr)
				cancel()

				// Wait before retrying
				select {
				case <-time.After(time.Duration(i+1) * time.Second):
				case <-ctx.Done():
					cancel()
					return ctx.Err()
				}
				continue
			}
			cancel()

			// Wait before retrying
			select {
			case <-time.After(time.Duration(i+1) * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return fmt.Errorf("Failed to send metric after %d attempts: %w", maxRetries, lastErr)
}
