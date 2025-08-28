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

func (m *Manager) GetBatch(bufferName string, maxCount int) ([]*protocol.Metric, error) {
	buf, err := m.GetOrCreateBuffer(bufferName, 1024)
	if err != nil {
		return nil, err
	}

	items, err := buf.GetBatch(maxCount)
	if err != nil {
		return nil, err
	}

	metrics := make([]*protocol.Metric, 0, len(items))
	for _, item := range items {
		if metric, ok := item.(*protocol.Metric); ok {
			metrics = append(metrics, metric)
		}
	}
	return metrics, nil
}

func (m *Manager) PutBatch(bufferName string, metrics []*protocol.Metric) error {
	buf, err := m.GetOrCreateBuffer(bufferName, 1024)
	if err != nil {
		return err
	}

	for _, metric := range metrics {
		if err := buf.Put(metric); err != nil {
			return err
		}
	}

	return nil
}

func (m *Manager) PutMetric(bufferName string, metric *protocol.Metric) error {
	buf, err := m.GetOrCreateBuffer(bufferName, 1024)
	if err != nil {
		return err
	}
	return buf.Put(metric)
}
