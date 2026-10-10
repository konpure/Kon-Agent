package plugin

import (
	"context"
	"time"
)

// MetricKind describes the aggregation semantics of an Event's value,
// mapped onto OTLP Gauge/Sum temporality in the protocol layer.
type MetricKind int

const (
	KindGauge MetricKind = iota
	KindSumCumulative
	KindSumDelta
	KindHistogram
)

// HistogramData carries an aggregated histogram (bucket counts instead of
// raw samples — Monarch distribution / OTLP Histogram: constant space and
// cross-instance aggregability).
type HistogramData struct {
	Count          uint64
	Sum            float64
	BucketCounts   []uint64
	ExplicitBounds []float64
}

type Event struct {
	Name   string
	Time   int64
	Labels map[string]string
	Values float64
	// Scope is the OTLP InstrumentationScope name, i.e. the plugin name.
	Scope string
	Kind  MetricKind
	// Histogram is set when Kind == KindHistogram.
	Histogram *HistogramData
}

type PluginFactory func(config PluginConfig) Plugin

type Plugin interface {
	Name() string
	Run(ctx context.Context, out chan<- Event) error
	Stop() error
	Config() PluginConfig
}

type PluginConfig struct {
	Enable     bool          `yaml:"enable"`
	Period     time.Duration `yaml:"period"`
	PluginType string        `yaml:"type"`
}
