package pipeline

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/konpure/Kon-Agent/internal/transport/buffer"
	"github.com/konpure/Kon-Agent/internal/transport/quic"
	"github.com/konpure/Kon-Agent/pkg/plugin"
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

	// Observability hooks, wired to the agent's StateManager.
	OnExported func(count int)
	OnError    func(errorType string)
}

// Pipeline is the explicit data path of the agent:
//
//	plugin(receiver) -> [batch] -> [sampling] -> exporter -> QUIC
//
// WAL will be inserted between the processors and the exporter.
type Pipeline struct {
	cfg        Config
	receiver   *Receiver
	batcher    *BatchProcessor
	processors []Processor
	exporter   *Exporter

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
	return &Pipeline{
		cfg:        cfg,
		receiver:   NewReceiver(events),
		batcher:    NewBatchProcessor(cfg.MaxBatchSize, cfg.MaxPending),
		processors: []Processor{NewSamplingProcessor()},
		exporter:   NewExporter(client, cfg.MaxRetries),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
}

// AddProcessor appends a stage after the batch processor.
func (p *Pipeline) AddProcessor(proc Processor) {
	p.processors = append(p.processors, proc)
}

// Run drives the pipeline until the event channel closes, stop is requested,
// or ctx is cancelled. Any pending batch is flushed before returning.
func (p *Pipeline) Run(ctx context.Context) {
	defer close(p.done)

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
			out, err := p.batcher.Process(ctx, []*buffer.Item{p.receiver.Convert(ev)})
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

// emit runs the remaining processors and exports the batch.
func (p *Pipeline) emit(ctx context.Context, batch []*buffer.Item) {
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
	if err := p.exporter.Export(ctx, req); err != nil {
		slog.Error("Failed to export batch", "error", err)
		p.onError("export_failed")
		// No WAL yet: hand the batch back for a later attempt.
		p.batcher.Requeue(batch)
		return
	}

	p.onExported(len(out))
}

// Shutdown stops the driver after flushing the pending batch.
func (p *Pipeline) Shutdown(ctx context.Context) error {
	p.once.Do(func() { close(p.stop) })
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
