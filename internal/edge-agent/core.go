package edge_agent

import (
	"context"
	"github.com/konpure/Kon-Agent/internal/config"
	"github.com/konpure/Kon-Agent/internal/pipeline"
	"github.com/konpure/Kon-Agent/internal/plugin"
	"github.com/konpure/Kon-Agent/internal/security"
	"github.com/konpure/Kon-Agent/internal/transport/quic"
	"log/slog"
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
}

func New(cfg *config.Config) *Core {
	pluginManager := plugin.NewManager(cfg)
	client := quic.NewClient(cfg.Server)

	// Init ResourceManager
	resourceManager := NewResourceManager(10*time.Second, 80.0)

	// Init StateManager
	stateManager := NewStateManager(cfg.Cache.Path + string(os.PathSeparator) + "agent_state.json")

	return &Core{
		cfg:             cfg,
		plugins:         pluginManager,
		client:          client,
		resourceManager: resourceManager,
		StateManager:    stateManager,
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

	slog.Info("Waiting for eBPF plugin initialization")

	initDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(initDeadline) {
		ebpfStatus := c.plugins.GetPluginStatus("ebpf")

		if ebpfStatus == plugin.StatusRunning || ebpfStatus == plugin.StatusError {
			slog.Info("eBPF plugin status", "status", ebpfStatus)
			break
		}

		time.Sleep(200 * time.Millisecond)
	}

	if err := security.DropPrivileges(); err != nil {
		slog.Warn("Failed to drop privileges, continuing with caution", "error", err)
	} else {
		slog.Info("Successfully dropped privileges to minimal set")
	}

	// Build the explicit pipeline: receiver -> [batch] -> [sampling] -> exporter.
	pipe := pipeline.New(pipeline.Config{
		AgentID:       c.cfg.ClientId,
		MaxBatchSize:  100,
		MaxPending:    1024,
		FlushInterval: 5 * time.Second,
		MaxRetries:    3,
		OnExported: func(count int) {
			c.StateManager.IncrementMetricSent()
			slog.Info("Successfully sent metrics batch", "data_points", count)
		},
		OnError: func(errorType string) {
			c.StateManager.RecordError(errorType)
		},
	}, c.plugins.Events(), c.client)

	go pipe.Run(ctx)

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

	// 1) Stop collection first so no new events are produced.
	c.plugins.Stop()

	// 2) Flush the tail batch and let the last export finish.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := pipe.Shutdown(shutdownCtx); err != nil {
		slog.Error("Pipeline shutdown did not complete", "error", err)
	}

	// 3) Close the transport only after the pipeline is drained.
	if err := c.client.Close(); err != nil {
		slog.Error("Failed to close QUIC connection", "error", err)
	}

	c.resourceManager.Stop()
	c.StateManager.Stop()

	slog.Info("Agent stopped")
	return nil
}

// (export retry logic now lives in internal/pipeline/exporter.go)
