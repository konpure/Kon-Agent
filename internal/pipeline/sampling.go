package pipeline

import "context"

// SamplingProcessor is a pass-through placeholder kept in the pipeline so the
// stage structure is final. It will be replaced by the BTrDB-style downsampling
// (5s raw -> 1min min/mean/max/count) and by the WAL backpressure degradation
// (5s -> 30s granularity) in later stages of the work.
type SamplingProcessor struct{}

func NewSamplingProcessor() *SamplingProcessor {
	return &SamplingProcessor{}
}

func (s *SamplingProcessor) Name() string {
	return "sampling"
}

func (s *SamplingProcessor) Process(_ context.Context, batch []*Item) ([]*Item, error) {
	return batch, nil
}
