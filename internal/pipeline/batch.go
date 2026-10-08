package pipeline

import (
	"context"
	"log/slog"
	"sync"
)

// BatchProcessor accumulates items and releases a batch when the size trigger
// fires; the time trigger is owned by the pipeline driver (see Take).
//
// It is the pipeline's first stage. With WAL disabled it also holds batches
// whose export failed; with WAL enabled durability is the WAL's job and
// requeue only covers WAL append failures.
type BatchProcessor struct {
	maxBatch   int
	maxPending int
	pending    []*Item
	mu         sync.Mutex
}

func NewBatchProcessor(maxBatch, maxPending int) *BatchProcessor {
	return &BatchProcessor{
		maxBatch:   maxBatch,
		maxPending: maxPending,
	}
}

func (b *BatchProcessor) Name() string {
	return "batch"
}

// Process appends incoming items and returns a batch once maxBatch is reached.
func (b *BatchProcessor) Process(_ context.Context, in []*Item) ([]*Item, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.append(in)
	if len(b.pending) >= b.maxBatch {
		return b.takeLocked(), nil
	}
	return nil, nil
}

// Take releases everything pending (time trigger or shutdown flush).
func (b *BatchProcessor) Take() []*Item {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.takeLocked()
}

// Requeue returns a failed batch to the front of the pending queue so it is
// retried before newer data.
func (b *BatchProcessor) Requeue(items []*Item) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.pending = append(items, b.pending...)
	if len(b.pending) > b.maxPending {
		dropped := len(b.pending) - b.maxPending
		// Keep the newest data: monitoring value degrades least that way.
		b.pending = b.pending[dropped:]
		slog.Warn("Pending queue overflow, dropped oldest items", "dropped", dropped)
	}
}

func (b *BatchProcessor) Pending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pending)
}

func (b *BatchProcessor) append(items []*Item) {
	b.pending = append(b.pending, items...)
	if len(b.pending) > b.maxPending {
		dropped := len(b.pending) - b.maxPending
		b.pending = b.pending[dropped:]
		slog.Warn("Pending queue overflow, dropped oldest items", "dropped", dropped)
	}
}

func (b *BatchProcessor) takeLocked() []*Item {
	if len(b.pending) == 0 {
		return nil
	}
	out := b.pending
	b.pending = nil
	return out
}
