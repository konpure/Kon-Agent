package pipeline

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/konpure/Kon-Agent/internal/aggregate"
)

// Sampling modes.
const (
	// SamplingOff always passes batches through unchanged.
	SamplingOff = "off"
	// SamplingAuto aggregates only while WAL backpressure is active
	// (graceful degradation: reduce granularity instead of blocking
	// collection — cf. Monarch; the 5s -> 30s drop of doc §7).
	SamplingAuto = "auto"
	// SamplingAlways always aggregates at the summary window
	// (BTrDB-style weak-network edge mode: ship only 1min summaries).
	SamplingAlways = "always"
)

// SamplingProcessor downsamples batches via the aggregate package.
// It holds window state, so it is a stateful Processor: batches are consumed
// into the window and an aggregated batch is emitted when the window closes
// (or on forced flush during shutdown).
type SamplingProcessor struct {
	mode           string
	degraded       *atomic.Bool
	summaryWindow  time.Duration
	degradedWindow time.Duration

	force atomic.Bool
	agg   *aggregate.Aggregator
}

func NewSamplingProcessor(mode string, summaryWindow, degradedWindow time.Duration, degraded *atomic.Bool) *SamplingProcessor {
	return &SamplingProcessor{
		mode:           mode,
		degraded:       degraded,
		summaryWindow:  summaryWindow,
		degradedWindow: degradedWindow,
	}
}

func (s *SamplingProcessor) Name() string {
	return "sampling"
}

// ForceFlush makes the next Process call emit the partial window regardless
// of the timer — used by the pipeline's tail flush so aggregated state is
// not lost on shutdown.
func (s *SamplingProcessor) ForceFlush() {
	s.force.Store(true)
}

func (s *SamplingProcessor) Process(_ context.Context, batch []*Item) ([]*Item, error) {
	window := s.effectiveWindow()

	// Pass-through mode: drain any partial window left by a previous mode.
	if window == 0 {
		if s.agg != nil && !s.agg.Empty() {
			flushed := s.flush()
			return append(flushed, batch...), nil
		}
		return batch, nil
	}

	var out []*Item
	// Window size changed (degradation toggled): emit the partial window
	// collected under the old granularity.
	if s.agg != nil && s.agg.Window() != window {
		out = s.flush()
		s.agg = nil
	}
	if s.agg == nil {
		s.agg = aggregate.New(window)
	}

	for _, it := range batch {
		if it == nil || it.Metric == nil {
			continue
		}
		s.agg.Add(it.Scope, it.Metric)
	}

	if s.agg.Due(time.Now()) || s.force.Swap(false) {
		out = append(out, s.flush()...)
	}
	return out, nil
}

// effectiveWindow returns 0 for pass-through, or the active window size.
func (s *SamplingProcessor) effectiveWindow() time.Duration {
	switch s.mode {
	case SamplingOff:
		return 0
	case SamplingAlways:
		return s.summaryWindow
	default: // auto
		if s.degraded != nil && s.degraded.Load() {
			return s.degradedWindow
		}
		return 0
	}
}

func (s *SamplingProcessor) flush() []*Item {
	results := s.agg.Flush()
	out := make([]*Item, 0, len(results))
	for _, r := range results {
		out = append(out, &Item{Scope: r.Scope, Metric: r.Metric})
	}
	return out
}
