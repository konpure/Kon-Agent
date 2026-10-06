package pipeline

import (
	"context"

	"github.com/konpure/Kon-Agent/internal/transport/buffer"
)

// Processor is one stage of the agent pipeline (batch pass-through semantics).
//
// It mirrors the OpenTelemetry Collector's receiver -> processor -> exporter
// model: a stage receives a batch of buffered items and returns the batch to
// be handed to the next stage. Returning an empty batch means the stage
// consumed everything (e.g. a downsampling stage still filling its window).
type Processor interface {
	Name() string
	Process(ctx context.Context, batch []*buffer.Item) ([]*buffer.Item, error)
}
