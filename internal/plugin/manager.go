package plugin

import (
	"context"
	"github.com/konpure/Kon-Agent/internal/config"
	kplugin "github.com/konpure/Kon-Agent/pkg/plugin"
	"log/slog"
	"sync"
	"time"
)

type PluginStatus string

const (
	StatusStopped PluginStatus = "stopped"
	StatusRunning PluginStatus = "running"
	StatusError   PluginStatus = "error"
	StatusUnknown PluginStatus = "unknown"
)

type pluginState struct {
	plugin kplugin.Plugin
	status PluginStatus
}

type PluginStatusChange struct {
	Name   string
	Status PluginStatus
	Time   time.Time
}

type Manager struct {
	plugins       []pluginState
	out           chan kplugin.Event
	statusChanges chan PluginStatusChange
	mu            sync.RWMutex
}

func (m *Manager) GetPluginNames() []string {
	return kplugin.GetRegisteredPlugins()
}

func (m *Manager) GetPluginStatus(name string) PluginStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, p := range m.plugins {
		if p.plugin.Name() == name {
			return p.status
		}
	}
	return StatusUnknown
}

func (m *Manager) PluginStatusChanges() <-chan PluginStatusChange {
	return m.statusChanges
}

// NewManager add plugins which is enabled
func NewManager(cfg *config.Config) *Manager {
	m := &Manager{
		out:           make(chan kplugin.Event, 100),
		statusChanges: make(chan PluginStatusChange, 10),
	}
	for name, pluginConf := range cfg.Plugins {
		if pluginConf.Enable {
			factory, exists := kplugin.GetFactory(name)
			if !exists {
				slog.Warn("Unknown plugin type", "name", name)
				continue
			}

			pluginInstance := factory(pluginConf)
			m.plugins = append(m.plugins, pluginState{plugin: pluginInstance, status: StatusStopped})
			slog.Info("Loaded plugin", "name", pluginInstance.Name())
		}
	}
	return m
}

// Start method run plugins in m.plugins
func (m *Manager) Start(ctx context.Context) {
	for i := range m.plugins {
		p := &m.plugins[i]
		go func(pluginState *pluginState) {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("plugin panic", "plugin", pluginState.plugin.Name(), "err", r)
					m.updatePluginStatus(pluginState.plugin.Name(), StatusError)
				} else {
					m.updatePluginStatus(pluginState.plugin.Name(), StatusStopped)
				}
			}()
			if err := pluginState.plugin.Run(ctx, m.out); err != nil {
				slog.Error("plugin exited with error", "plugin", pluginState.plugin.Name(), "err", err)
				m.updatePluginStatus(pluginState.plugin.Name(), StatusError)
			}
		}(p)
	}
}

func (m *Manager) updatePluginStatus(name string, status PluginStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.plugins {
		if m.plugins[i].plugin.Name() == name {
			m.plugins[i].status = status
			// Send status change
			m.statusChanges <- PluginStatusChange{
				Name:   name,
				Status: status,
				Time:   time.Now(),
			}
			break
		}
	}
}

func (m *Manager) Stop() {
	for _, p := range m.plugins {
		_ = p.plugin.Stop()
	}
	close(m.out)
}

func (m *Manager) Events() <-chan kplugin.Event {
	return m.out
}
