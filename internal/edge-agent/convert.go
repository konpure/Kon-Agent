package edge_agent

import (
	"os"
	"time"

	"github.com/konpure/Kon-Agent/internal/transport/buffer"
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
}

// eventToMetric converts a plugin Event into a protocol v2 Metric carrying
// exactly one data point. The OTLP data type is derived from Event.Kind.
func eventToMetric(e plugin.Event) *protocol.Metric {
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
	default:
		m.Data = &protocol.Metric_Gauge{Gauge: &protocol.Gauge{
			DataPoints: []*protocol.NumberDataPoint{dp},
		}}
	}
	return m
}

// buildExportRequest assembles the OTLP-style three-layer request:
// Resource -> ScopeMetrics -> Metric. Data points of the same metric
// (e.g. multiple disk partitions) are merged into one Metric message.
func buildExportRequest(agentID string, items []*buffer.Item) *protocol.ExportMetricsRequest {
	req := &protocol.ExportMetricsRequest{
		Resource: &protocol.Resource{
			AgentId:    agentID,
			Attributes: resourceAttributes(),
		},
		ExportTimeUnixNano: time.Now().UnixNano(),
	}

	type metricKey struct {
		scope string
		name  string
	}
	scopes := make(map[string]*protocol.ScopeMetrics)
	metricIdx := make(map[metricKey]int)
	var scopeOrder []string

	for _, it := range items {
		if it == nil || it.Metric == nil {
			continue
		}
		sm, ok := scopes[it.Scope]
		if !ok {
			sm = &protocol.ScopeMetrics{
				Scope: &protocol.InstrumentationScope{Name: it.Scope},
			}
			scopes[it.Scope] = sm
			scopeOrder = append(scopeOrder, it.Scope)
		}
		key := metricKey{scope: it.Scope, name: it.Metric.Name}
		if idx, ok := metricIdx[key]; ok {
			mergeDataPoints(sm.Metrics[idx], it.Metric)
		} else {
			metricIdx[key] = len(sm.Metrics)
			sm.Metrics = append(sm.Metrics, it.Metric)
		}
	}

	for _, name := range scopeOrder {
		req.ScopeMetrics = append(req.ScopeMetrics, scopes[name])
	}
	return req
}

// mergeDataPoints appends src's data points into dst. Both metrics are
// guaranteed to share the same name and data kind by construction.
func mergeDataPoints(dst, src *protocol.Metric) {
	switch d := dst.Data.(type) {
	case *protocol.Metric_Gauge:
		if s, ok := src.Data.(*protocol.Metric_Gauge); ok {
			d.Gauge.DataPoints = append(d.Gauge.DataPoints, s.Gauge.DataPoints...)
		}
	case *protocol.Metric_Sum:
		if s, ok := src.Data.(*protocol.Metric_Sum); ok {
			d.Sum.DataPoints = append(d.Sum.DataPoints, s.Sum.DataPoints...)
		}
	case *protocol.Metric_Histogram:
		if s, ok := src.Data.(*protocol.Metric_Histogram); ok {
			d.Histogram.DataPoints = append(d.Histogram.DataPoints, s.Histogram.DataPoints...)
		}
	}
}

// resourceAttributes returns low-cardinality resource attributes only
// (tag/field discipline: no high-cardinality values such as connection_id).
func resourceAttributes() map[string]string {
	attrs := make(map[string]string)
	if hostname, err := os.Hostname(); err == nil {
		attrs["host.name"] = hostname
	}
	return attrs
}
