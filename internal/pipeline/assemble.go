package pipeline

import (
	"os"
	"time"

	"github.com/konpure/Kon-Agent/internal/transport/buffer"
	"github.com/konpure/Kon-Agent/pkg/protocol"
)

// AssembleExportRequest builds the OTLP-style three-layer request:
// Resource -> ScopeMetrics -> Metric. Data points of the same metric
// (e.g. several disk partitions) are merged into one Metric message.
func AssembleExportRequest(agentID string, items []*buffer.Item) *protocol.ExportMetricsRequest {
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

// mergeDataPoints appends src's data points into dst.
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
