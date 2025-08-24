package edge_agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type AgentState struct {
	LastConnected time.Time         `json:"last_connected"`
	PluginsStatus map[string]string `json:"plugins_status"`
	Connections   int               `json:"connections"`
	MetricsSent   int64             `json:"metrics_sent"`
	Errors        map[string]int    `json:"errors"`
}

type StateManager struct {
	ctx       context.Context
	cancel    context.CancelFunc
	state     AgentState
	mutex     sync.RWMutex
	stateFile string
}

func NewStateManager(stateFile string) *StateManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &StateManager{
		ctx:    ctx,
		cancel: cancel,
		state: AgentState{
			PluginsStatus: make(map[string]string),
			Errors:        make(map[string]int),
		},
		stateFile: stateFile,
	}
}

func (sm *StateManager) Start() {
	// TODO: Implement
	slog.Info("Starting state manager", "state_file", sm.stateFile)

	sm.loadState()

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if err := sm.persistState(); err != nil {
					slog.Error("Failed to persist state", "error", err)
				}
			case <-sm.ctx.Done():
				sm.persistState()
				slog.Info("State manager stopped")
				return
			}
		}
	}()
}

func (sm *StateManager) Stop() {
	sm.cancel()
}

func (sm *StateManager) UpdatePluginStatus(pluginName, status string) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	sm.state.PluginsStatus[pluginName] = status
}

func (sm *StateManager) UpdateConnectionState(connected bool) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	if connected {
		sm.state.LastConnected = time.Now()
		sm.state.Connections++
	}
}

func (sm *StateManager) IncrementMetricSent() {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	sm.state.MetricsSent++
}

func (sm *StateManager) RecordError(errorType string) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	sm.state.Errors[errorType]++
}

func (sm *StateManager) GetState() AgentState {
	sm.mutex.RLock()
	defer sm.mutex.RUnlock()

	return sm.state
}

func (sm *StateManager) persistState() error {
	dir := filepath.Dir(sm.stateFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	file, err := os.Create(sm.stateFile)
	if err != nil {
		return err
	}
	defer file.Close()

	sm.mutex.RLock()
	defer sm.mutex.RUnlock()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", " ")
	return encoder.Encode(sm.state)
}

func (sm *StateManager) loadState() {
	if _, err := os.Stat(sm.stateFile); os.IsNotExist(err) {
		slog.Info("State file not found, starting with fresh state")
		return
	}

	file, err := os.Open(sm.stateFile)
	if err != nil {
		slog.Warn("Failed to open state file", "error", err)
		return
	}
	defer file.Close()

	var loadedState AgentState
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&loadedState); err != nil {
		slog.Warn("Failed to decode state file", "error", err)
		return
	}

	sm.mutex.Lock()
	sm.state = loadedState

	sm.state.Connections = 0
	sm.state.MetricsSent = 0
	sm.mutex.Unlock()

	slog.Info("Loaded state from file", "state", sm.state)
}
