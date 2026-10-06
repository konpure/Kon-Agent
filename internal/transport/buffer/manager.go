package buffer

import (
	"sync"

	"github.com/konpure/Kon-Agent/pkg/protocol"
)

// Item is a buffered metric together with its instrumentation scope
// (the producing plugin's name).
type Item struct {
	Scope  string
	Metric *protocol.Metric
}

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

func (m *Manager) GetBatch(bufferName string, maxCount int) ([]*Item, error) {
	buf, err := m.GetOrCreateBuffer(bufferName, 1024)
	if err != nil {
		return nil, err
	}

	items, err := buf.GetBatch(maxCount)
	if err != nil {
		return nil, err
	}

	result := make([]*Item, 0, len(items))
	for _, item := range items {
		if it, ok := item.(*Item); ok {
			result = append(result, it)
		}
	}
	return result, nil
}

func (m *Manager) PutBatch(bufferName string, items []*Item) error {
	buf, err := m.GetOrCreateBuffer(bufferName, 1024)
	if err != nil {
		return err
	}

	for _, item := range items {
		if err := buf.Put(item); err != nil {
			return err
		}
	}

	return nil
}

func (m *Manager) PutMetric(bufferName string, item *Item) error {
	buf, err := m.GetOrCreateBuffer(bufferName, 1024)
	if err != nil {
		return err
	}
	return buf.Put(item)
}
