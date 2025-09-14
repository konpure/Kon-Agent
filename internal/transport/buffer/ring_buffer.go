package buffer

import (
	"errors"
	"fmt"
	"sync"
)

type RingBuffer struct {
	buffer []interface{}
	size   int
	head   int
	tail   int
	count  int
	mu     sync.Mutex
}

func NewRingBuffer(size int) (*RingBuffer, error) {
	if size <= 0 {
		return nil, errors.New("buffer size must be positive")
	}
	return &RingBuffer{
		buffer: make([]interface{}, size),
		size:   size,
	}, nil
}

func (r *RingBuffer) Put(item interface{}) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.count == r.size {
		return errors.New("buffer is full")
	}

	r.buffer[r.head] = item
	r.head = (r.head + 1) % r.size
	r.count++
	return nil
}

func (r *RingBuffer) Get() (interface{}, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.count == 0 {
		return nil, errors.New("buffer is empty")
	}

	item := r.buffer[r.tail]
	r.tail = (r.tail + 1) % r.size
	r.count--
	return item, nil
}

func (r *RingBuffer) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

func (r *RingBuffer) Capacity() int {
	return r.size
}

func (r *RingBuffer) GetBatch(count int) ([]interface{}, error) {
	if count <= 0 || count > r.Capacity() {
		return nil, fmt.Errorf("invalid count: %d", count)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.size == 0 {
		return nil, fmt.Errorf("buffer is empty")
	}

	actualCount := min(count, r.count)
	result := make([]interface{}, 0, actualCount)

	for i := 0; i < actualCount; i++ {
		index := (r.tail + i) % r.Capacity()
		result = append(result, r.buffer[index])
	}

	r.tail = (r.tail + actualCount) % r.Capacity()
	r.count -= actualCount

	return result, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
