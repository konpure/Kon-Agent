package plugin

import (
	"github.com/konpure/Kon-Agent/internal/config"
	"github.com/konpure/Kon-Agent/internal/plugin/system/cpu"
	"github.com/konpure/Kon-Agent/pkg/plugin"
	"log/slog"
)

type Manager struct {
	plugins []plugin.Plugin
	out     chan plugin.Event
}

// NewManager add plugins which is enabled
func NewManager(cfg *config.Config) *Manager {
	m := &Manager{out: make(chan plugin.Event, 100)}
	if cfg.Plugins.CPU.Enable {
		m.plugins = append(m.plugins, cpu.New())
	}
	return m
}

// Start method run plugins in m.plugins
func (m *Manager) Start() {
	for _, p := range m.plugins {
		go func(p1 plugin.Plugin) {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("plugin panic", "plugin", p1.Name(), "err", r)
				}
			}()
			if err := p1.Run(m.out); err != nil {
				slog.Error("plugin exited with error", "plugin", p1.Name(), "err", err)
			}
		}(p)
	}
}

func (m *Manager) Stop() {
	for _, p := range m.plugins {
		_ = p.Stop()
	}
	close(m.out)
}

func (m *Manager) Events() <-chan plugin.Event {
	return m.out
}
