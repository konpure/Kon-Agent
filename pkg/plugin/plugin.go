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

type Event struct {
	Name   string
	Time   int64
	Labels map[string]string
	Values float64
	// Scope is the OTLP InstrumentationScope name, i.e. the plugin name.
	Scope string
	Kind  MetricKind
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
