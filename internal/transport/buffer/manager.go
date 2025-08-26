package buffer

import (
	"github.com/konpure/Kon-Agent/pkg/protocol"
	"sync"
)

type Manager struct {
	buffers map[string]*RingBuffer
	mu      sync.RWMutex
}

func NewManager() *Manager {
	return &Manager{
		buffers: make(map[string]*RingBuffer),
	}
}

func (m *Manager) GetOrCreateBuffer(name string, size int) (*RingBuffer, error) {
	m.mu.RLock()
	buf, exists := m.buffers[name]
	m.mu.RUnlock()

	if exists {
		return buf, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if buf, exists := m.buffers[name]; exists {
		return buf, nil
	}

	newBuf, err := NewRingBuffer(size)
	if err != nil {
		return nil, err
	}
	m.buffers[name] = newBuf
	return newBuf, nil
}

func (m *Manager) PutMetric(bufferName string, metric *protocol.Metric) error {
	buf, err := m.GetOrCreateBuffer(bufferName, 1024)
	if err != nil {
		return err
	}
	return buf.Put(metric)
}
