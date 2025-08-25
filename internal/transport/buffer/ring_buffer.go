package buffer

import (
	"errors"
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
