package pipeline

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/konpure/Kon-Agent/internal/transport/codec"
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
	// CodecMode selects the wire encoding (raw/snappy/gorilla).
	CodecMode codec.Mode

	// SamplingMode: "off" | "auto" | "always" (see SamplingProcessor).
	SamplingMode string
	// SummaryWindow aggregates raw points when mode=always (BTrDB-style summary).
	SummaryWindow time.Duration
	// DegradedWindow is the coarser granularity under WAL backpressure.
	DegradedWindow time.Duration

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
	sampling   *SamplingProcessor
	processors []Processor
	exporter   *Exporter
	consumer   *walConsumer // nil when WAL is disabled
	degraded   *atomic.Bool // set by the WAL consumer's backpressure monitor

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
	if cfg.SamplingMode == "" {
		cfg.SamplingMode = SamplingAuto
	}
	if cfg.SummaryWindow <= 0 {
		cfg.SummaryWindow = time.Minute
	}
	if cfg.DegradedWindow <= 0 {
		cfg.DegradedWindow = 30 * time.Second
	}

	p := &Pipeline{
		cfg:      cfg,
		receiver: NewReceiver(events),
		batcher:  NewBatchProcessor(cfg.MaxBatchSize, cfg.MaxPending),
		exporter: NewExporter(client, cfg.MaxRetries, cfg.CodecMode),
		degraded: &atomic.Bool{},
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	p.sampling = NewSamplingProcessor(cfg.SamplingMode, cfg.SummaryWindow, cfg.DegradedWindow, p.degraded)
	p.processors = []Processor{p.sampling}

	if cfg.WAL != nil {
		// The backpressure flag feeds both the sampling processor (via the
		// shared atomic) and the user's observability hook.
		onPressure := func(degraded bool) {
			p.degraded.Store(degraded)
			if cfg.OnBackpressure != nil {
				cfg.OnBackpressure(degraded)
			}
		}
		consumer, err := newWALConsumer(cfg.WAL, p.exporter, cfg.OnError, onPressure)
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
				p.emitTail(ctx)
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
			p.emitTail(ctx)
			return

		case <-ctx.Done():
			p.emitTail(ctx)
			return
		}
	}
}

// emitTail flushes the batcher AND forces the sampling window to emit its
// partial aggregation, so shutdown does not lose in-window data.
func (p *Pipeline) emitTail(ctx context.Context) {
	p.sampling.ForceFlush()
	p.emit(ctx, p.batcher.Take())
	// The force flush above only takes effect when Process runs; if the
	// batcher was empty, Process never ran — flush explicitly.
	if p.sampling != nil {
		if out, err := p.sampling.Process(ctx, nil); err == nil && len(out) > 0 {
			p.emit(ctx, out)
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
	if err := p.exporter.ExportBatch(ctx, payload); err != nil {
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
