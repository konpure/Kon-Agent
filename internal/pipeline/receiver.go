package pipeline

import (
	"log/slog"

	"github.com/konpure/Kon-Agent/pkg/plugin"
	"github.com/konpure/Kon-Agent/pkg/protocol"
)

// metricUnits maps metric names to UCUM unit annotations (OTLP Metric.unit).
// References: OTLP metrics data model; node_exporter base-unit conventions.
var metricUnits = map[string]string{
	"cpu_usage_percent":         "%",
	"memory_usage_percent":      "%",
	"memory_usage_bytes":        "By",
	"memory_total_bytes":        "By",
	"memory_swap_usage_percent": "%",
	"memory_swap_usage_bytes":   "By",
	"disk_usage_percent":        "%",
	"disk_usage_bytes":          "By",
	"disk_total_bytes":          "By",
	"disk_free_bytes":           "By",
	"disk_io_read_bytes_total":  "By",
	"disk_io_write_bytes_total": "By",
	"disk_io_read_count_total":  "{io}",
	"disk_io_write_count_total": "{io}",
	"disk_io_read_bytes_delta":  "By",
	"disk_io_write_bytes_delta": "By",
	"disk_io_read_count_delta":  "{io}",
	"disk_io_write_count_delta": "{io}",
	"network_packets_total":     "{packet}",
	"tcp_rtt_seconds":           "s",
}

// Receiver is the pipeline's source stage. It owns the plugin event channel
// and converts plugin Events into protocol v2 metrics.
type Receiver struct {
	events <-chan plugin.Event
}

func NewReceiver(events <-chan plugin.Event) *Receiver {
	return &Receiver{events: events}
}

func (r *Receiver) Events() <-chan plugin.Event {
	return r.events
}

// Convert turns a plugin Event into a buffered item carrying its scope.
func (r *Receiver) Convert(e plugin.Event) *Item {
	slog.Debug("Received event",
		"event", e.Name,
		"value", e.Values,
		"labels", e.Labels)

	return &Item{
		Scope:  e.Scope,
		Metric: EventToMetric(e),
	}
}

// EventToMetric converts a plugin Event into a protocol v2 Metric holding
// exactly one data point. The OTLP data type derives from Event.Kind.
func EventToMetric(e plugin.Event) *protocol.Metric {
	dp := &protocol.NumberDataPoint{
		Attributes:   e.Labels,
		TimeUnixNano: e.Time,
		Value:        e.Values,
	}

	m := &protocol.Metric{
		Name: e.Name,
		Unit: metricUnits[e.Name],
	}

	switch e.Kind {
	case plugin.KindSumCumulative:
		m.Data = &protocol.Metric_Sum{Sum: &protocol.Sum{
			DataPoints:  []*protocol.NumberDataPoint{dp},
			Temporality: protocol.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
			IsMonotonic: true,
		}}
	case plugin.KindSumDelta:
		m.Data = &protocol.Metric_Sum{Sum: &protocol.Sum{
			DataPoints:  []*protocol.NumberDataPoint{dp},
			Temporality: protocol.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
			IsMonotonic: true,
		}}
	case plugin.KindHistogram:
		m.Data = histogramData(e)
	default:
		m.Data = &protocol.Metric_Gauge{Gauge: &protocol.Gauge{
			DataPoints: []*protocol.NumberDataPoint{dp},
		}}
	}
	return m
}

// histogramData builds the protocol Histogram from a plugin histogram event.
// Temporality is delta: producers (e.g. the eBPF plugin) read-and-clear the
// kernel maps every period. min/max stay unset — they are not recoverable
// from aggregated bucket counters, and OTLP marks them optional.
func histogramData(e plugin.Event) *protocol.Metric_Histogram {
	hp := &protocol.HistogramDataPoint{
		Attributes:   e.Labels,
		TimeUnixNano: e.Time,
	}
	if h := e.Histogram; h != nil {
		hp.Count = h.Count
		hp.Sum = h.Sum
		hp.BucketCounts = h.BucketCounts
		hp.ExplicitBounds = h.ExplicitBounds
	}
	return &protocol.Metric_Histogram{Histogram: &protocol.Histogram{
		DataPoints:  []*protocol.HistogramDataPoint{hp},
		Temporality: protocol.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
	}}
}
