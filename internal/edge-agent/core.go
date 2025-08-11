package edge_agent

import (
	"context"
	"github.com/konpure/Kon-Agent/internal/config"
	"github.com/konpure/Kon-Agent/internal/plugin"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

type Core struct {
	cfg     *config.Config
	plugins *plugin.Manager
}

func New(cfg *config.Config) *Core {
	pluginManager := plugin.NewManager(cfg)

	return &Core{
		cfg:     cfg,
		plugins: pluginManager,
	}
}

func (c *Core) Run() error {
	slog.Info("Agent started", "config", c.cfg)

	c.plugins.Start()

	// Setup signal handling for shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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
