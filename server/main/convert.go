package main

import (
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/konpure/Kon-Agent/pkg/protocol"
)

// vmSample is one JSON line of the VictoriaMetrics /api/v1/import format.
type vmSample struct {
	Metric     map[string]string `json:"metric"`
	Values     []float64         `json:"values"`
	Timestamps []int64           `json:"timestamps"` // milliseconds
}

// deltaAccumulator converts delta-temporality series into cumulative ones.
// The agent reports deltas (read-then-clear on the eBPF maps); the
// VictoriaMetrics/Prometheus ecosystem expects cumulative counters, so the
// server accumulates per series. State lives for the server process —
// a server restart resets counters, which is normal in this ecosystem.
type deltaAccumulator struct {
	mu    sync.Mutex
	sums  map[string]float64
	hists map[string]*histAcc
}

type histAcc struct {
	bounds  []float64
	buckets []uint64
	count   uint64
	sum     float64
}

func newDeltaAccumulator() *deltaAccumulator {
	return &deltaAccumulator{
		sums:  make(map[string]float64),
		hists: make(map[string]*histAcc),
	}
}

func (a *deltaAccumulator) addSum(key string, delta float64) float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sums[key] += delta
	return a.sums[key]
}

func (a *deltaAccumulator) addHistogram(key string, hp *protocol.HistogramDataPoint) *histAcc {
	a.mu.Lock()
	defer a.mu.Unlock()

	acc, ok := a.hists[key]
	if !ok || !sameFloatSlice(acc.bounds, hp.GetExplicitBounds()) {
		acc = &histAcc{
			bounds:  append([]float64(nil), hp.GetExplicitBounds()...),
			buckets: make([]uint64, len(hp.GetBucketCounts())),
		}
		a.hists[key] = acc
	}
	for i, c := range hp.GetBucketCounts() {
		if i < len(acc.buckets) {
			acc.buckets[i] += c
		}
	}
	acc.count += hp.GetCount()
	acc.sum += hp.GetSum()
	return acc
}

// convertToVMLines flattens one export request into VM import lines.
//
// Mapping rules:
//   - Gauge              -> the metric itself
//   - Sum cumulative     -> the metric itself (counter)
//   - Sum delta          -> accumulated into a cumulative counter first
//   - Histogram w/bounds -> Prometheus-style name_bucket{le=...} + name_sum + name_count
//   - Histogram w/o bounds (downsampled gauge summary)
//     -> name_min / name_max / name_avg / name_count gauges
func convertToVMLines(req *protocol.ExportMetricsRequest, acc *deltaAccumulator) []vmSample {
	var out []vmSample

	res := req.GetResource()
	baseLabels := map[string]string{"agent_id": res.GetAgentId()}
	for k, v := range res.GetAttributes() {
		baseLabels[sanitizeLabel(k)] = v
	}

	emit := func(name string, labels map[string]string, value float64, tsNanos int64) {
		merged := make(map[string]string, len(baseLabels)+len(labels)+1)
		for k, v := range baseLabels {
			merged[k] = v
		}
		for k, v := range labels {
			merged[sanitizeLabel(k)] = v
		}
		merged["__name__"] = name
		out = append(out, vmSample{
			Metric:     merged,
			Values:     []float64{value},
			Timestamps: []int64{tsNanos / 1e6},
		})
	}

	for _, sm := range req.GetScopeMetrics() {
		scope := sm.GetScope().GetName()
		for _, m := range sm.GetMetrics() {
			name := m.GetName()
			switch d := m.GetData().(type) {
			case *protocol.Metric_Gauge:
				for _, dp := range d.Gauge.GetDataPoints() {
					emit(name, withScope(dp.GetAttributes(), scope), dp.GetValue(), dp.GetTimeUnixNano())
				}

			case *protocol.Metric_Sum:
				delta := d.Sum.GetTemporality() == protocol.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA
				for _, dp := range d.Sum.GetDataPoints() {
					labels := withScope(dp.GetAttributes(), scope)
					value := dp.GetValue()
					if delta {
						value = acc.addSum(seriesKey(name, labels), value)
					}
					emit(name, labels, value, dp.GetTimeUnixNano())
				}

			case *protocol.Metric_Histogram:
				for _, hp := range d.Histogram.GetDataPoints() {
					labels := withScope(hp.GetAttributes(), scope)
					if len(hp.GetExplicitBounds()) == 0 {
						// Downsampled gauge summary: per-window stats.
						emit(name+"_min", labels, hp.GetMin(), hp.GetTimeUnixNano())
						emit(name+"_max", labels, hp.GetMax(), hp.GetTimeUnixNano())
						if hp.GetCount() > 0 {
							emit(name+"_avg", labels, hp.GetSum()/float64(hp.GetCount()), hp.GetTimeUnixNano())
						}
						emit(name+"_count", labels, float64(hp.GetCount()), hp.GetTimeUnixNano())
						continue
					}
					// Real histogram: accumulate delta buckets, emit
					// Prometheus-style cumulative bucket series.
					cum := acc.addHistogram(seriesKey(name, labels), hp)
					for i, bound := range cum.bounds {
						if i >= len(cum.buckets) {
							break
						}
						bl := copyLabels(labels)
						bl["le"] = strconv.FormatFloat(bound, 'g', -1, 64)
						emit(name+"_bucket", bl, float64(cum.buckets[i]), hp.GetTimeUnixNano())
					}
					inf := copyLabels(labels)
					inf["le"] = "+Inf"
					emit(name+"_bucket", inf, float64(cum.count), hp.GetTimeUnixNano())
					emit(name+"_sum", labels, cum.sum, hp.GetTimeUnixNano())
					emit(name+"_count", labels, float64(cum.count), hp.GetTimeUnixNano())
				}
			}
		}
	}
	return out
}

func withScope(attrs map[string]string, scope string) map[string]string {
	m := make(map[string]string, len(attrs)+1)
	for k, v := range attrs {
		m[k] = v
	}
	m["plugin"] = scope
	return m
}

func copyLabels(m map[string]string) map[string]string {
	out := make(map[string]string, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// sanitizeLabel makes OTLP-style attribute keys valid Prometheus label names
// (host.name -> host_name).
func sanitizeLabel(k string) string {
	return strings.NewReplacer(".", "_", "-", "_").Replace(k)
}

// seriesKey builds a stable accumulator key from a metric name and labels.
func seriesKey(name string, labels map[string]string) string {
	var sb strings.Builder
	sb.WriteString(name)
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sb.WriteByte(0)
		sb.WriteString(k)
		sb.WriteByte(1)
		sb.WriteString(labels[k])
	}
	return sb.String()
}

func sameFloatSlice(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
