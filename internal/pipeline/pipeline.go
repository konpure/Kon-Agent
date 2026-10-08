package pipeline

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/konpure/Kon-Agent/internal/transport/quic"
	"github.com/konpure/Kon-Agent/internal/wal"
	"github.com/konpure/Kon-Agent/pkg/plugin"
	"google.golang.org/protobuf/proto"
)

// Config holds the pipeline parameters.
type Config struct {
	AgentID string
	// MaxBatchSize triggers a flush once reached.
	MaxBatchSize int
	// MaxPending bounds the in-memory queue; oldest items are dropped beyond it.
	MaxPending int
	// FlushInterval is the time trigger owned by the driver.
	FlushInterval time.Duration
	// MaxRetries bounds exporter retries per batch.
	MaxRetries int

	// WAL, when set, makes the pipeline write-through: every batch is
	// persisted before sending, and a consumer goroutine replays the log.
	// When nil, the pipeline falls back to direct export + in-memory requeue.
	WAL *wal.WAL

	// Observability hooks, wired to the agent's StateManager.
	OnExported     func(count int)
	OnError        func(errorType string)
	OnBackpressure func(degraded bool)
}

// Pipeline is the explicit data path of the agent:
//
//	plugin(receiver) -> [batch] -> [sampling] -> [WAL] -> exporter -> QUIC
type Pipeline struct {
	cfg        Config
	receiver   *Receiver
	batcher    *BatchProcessor
	processors []Processor
	exporter   *Exporter
	consumer   *walConsumer // nil when WAL is disabled

	stop chan struct{}
	done chan struct{}
	once sync.Once
}

func New(cfg Config, events <-chan plugin.Event, client *quic.Client) *Pipeline {
	if cfg.MaxBatchSize <= 0 {
		cfg.MaxBatchSize = 100
	}
	if cfg.MaxPending <= 0 {
		cfg.MaxPending = 1024
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 5 * time.Second
	}

	p := &Pipeline{
		cfg:        cfg,
		receiver:   NewReceiver(events),
		batcher:    NewBatchProcessor(cfg.MaxBatchSize, cfg.MaxPending),
		processors: []Processor{NewSamplingProcessor()},
		exporter:   NewExporter(client, cfg.MaxRetries),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}

	if cfg.WAL != nil {
		consumer, err := newWALConsumer(cfg.WAL, p.exporter, cfg.OnError, cfg.OnBackpressure)
		if err != nil {
			// Degrade to direct export rather than failing the whole agent.
			slog.Error("Failed to init WAL consumer, falling back to direct export", "error", err)
			p.onError("wal_consumer_init_failed")
		} else {
			p.consumer = consumer
		}
	}
	return p
}

// AddProcessor appends a stage after the batch processor.
func (p *Pipeline) AddProcessor(proc Processor) {
	p.processors = append(p.processors, proc)
}

// Run drives the pipeline until the event channel closes, stop is requested,
// or ctx is cancelled. Any pending batch is flushed before returning.
func (p *Pipeline) Run(ctx context.Context) {
	defer close(p.done)

	if p.consumer != nil {
		go p.consumer.run(ctx)
	}

	ticker := time.NewTicker(p.cfg.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case ev, ok := <-p.receiver.Events():
			if !ok {
				slog.Info("Event channel closed, flushing tail batch")
				p.emit(ctx, p.batcher.Take())
				return
			}
			out, err := p.batcher.Process(ctx, []*Item{p.receiver.Convert(ev)})
			if err != nil {
				p.onError("batch_failed")
				continue
			}
			p.emit(ctx, out)

		case <-ticker.C:
			p.emit(ctx, p.batcher.Take())

		case <-p.stop:
			p.emit(ctx, p.batcher.Take())
			return

		case <-ctx.Done():
			p.emit(ctx, p.batcher.Take())
			return
		}
	}
}

// emit runs the processors, then either persists the batch into the WAL
// (write-through) or exports it directly (fallback).
func (p *Pipeline) emit(ctx context.Context, batch []*Item) {
	if len(batch) == 0 {
		return
	}

	out := batch
	for _, proc := range p.processors {
		var err error
		out, err = proc.Process(ctx, out)
		if err != nil {
			slog.Error("Processor failed", "processor", proc.Name(), "error", err)
			p.onError("processor_" + proc.Name())
			p.batcher.Requeue(batch)
			return
		}
	}
	if len(out) == 0 {
		return
	}

	req := AssembleExportRequest(p.cfg.AgentID, out)
	payload, err := proto.Marshal(req)
	if err != nil {
		slog.Error("Failed to marshal export request", "error", err)
		p.onError("marshal_failed")
		return
	}

	if p.consumer != nil {
		if err := p.cfg.WAL.Append(0, payload); err != nil {
			slog.Error("Failed to append batch to WAL", "error", err)
			p.onError("wal_append_failed")
			p.batcher.Requeue(batch)
			return
		}
		p.consumer.notify()
		// Durable now; the consumer delivers it. Count as accepted.
		p.onExported(len(out))
		return
	}

	// Fallback without WAL: direct export, requeue on failure.
	if err := p.exporter.ExportBytes(ctx, payload); err != nil {
		slog.Error("Failed to export batch", "error", err)
		p.onError("export_failed")
		p.batcher.Requeue(batch)
		return
	}
	p.onExported(len(out))
}

// Shutdown stops the driver after flushing the pending batch, then drains
// and stops the WAL consumer.
func (p *Pipeline) Shutdown(ctx context.Context) error {
	p.once.Do(func() { close(p.stop) })
	select {
	case <-p.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if p.consumer != nil {
		p.consumer.stop()
	}
	return nil
}

func (p *Pipeline) onExported(count int) {
	if p.cfg.OnExported != nil {
		p.cfg.OnExported(count)
	}
}

func (p *Pipeline) onError(errorType string) {
	if p.cfg.OnError != nil {
		p.cfg.OnError(errorType)
	}
}
