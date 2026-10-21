// Package aggregate implements agent-side downsampling: raw points are
// accumulated per series over a time window and emitted as
// (min/mean/max/count) summaries.
//
// This is a two-level simplification of BTrDB's time-partitioned aggregation
// tree (FAST 2016), adapted to edge resource constraints: keep raw 5s data
// locally, and under backpressure (or weak-network mode) only ship the
// windowed summary. The summary rides the protocol's Histogram type — a
// degenerate distribution — so no new wire type is needed (cf. Monarch's
// distribution as a first-class citizen).
package aggregate

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/konpure/Kon-Agent/pkg/protocol"
)

// metricKind mirrors the protocol oneof branches.
type metricKind int

const (
	kindGauge metricKind = iota
	kindSum
	kindHistogram
)

// Result is one aggregated series ready to re-enter the pipeline.
type Result struct {
	Scope  string
	Metric *protocol.Metric
}

// seriesStats accumulates one series within a window.
type seriesStats struct {
	scope       string
	name        string
	unit        string
	desc        string
	attrs       map[string]string
	kind        metricKind
	temporality protocol.AggregationTemporality
	monotonic   bool

	firstTS int64
	lastTS  int64
	count   uint64
	sum     float64
	min     float64
	max     float64

	// Sum handling.
	deltaTotal float64 // delta sums: accumulated window total
	lastValue  float64 // cumulative sums: latest value

	// Histogram merging.
	bucketCounts []uint64
	bounds       []float64
}

// Aggregator holds per-series window state.
type Aggregator struct {
	window      time.Duration
	windowStart time.Time
	series      map[string]*seriesStats
	order       []string
}

func New(window time.Duration) *Aggregator {
	return &Aggregator{
		window: window,
		series: make(map[string]*seriesStats),
	}
}

// Window returns the configured window size.
func (a *Aggregator) Window() time.Duration {
	return a.window
}

// Empty reports whether no points have been accumulated since the last flush.
func (a *Aggregator) Empty() bool {
	return len(a.series) == 0
}

// Due reports whether the window has elapsed since the first point of the
// current window arrived.
func (a *Aggregator) Due(now time.Time) bool {
	return !a.windowStart.IsZero() && !now.Before(a.windowStart.Add(a.window))
}

// Add consumes one metric (typically holding a single data point, as produced
// by the pipeline receiver).
func (a *Aggregator) Add(scope string, m *protocol.Metric) {
	switch d := m.GetData().(type) {
	case *protocol.Metric_Gauge:
		for _, dp := range d.Gauge.GetDataPoints() {
			a.touchWindow(dp.GetTimeUnixNano())
			st := a.seriesFor(scope, m, dp.GetAttributes(), kindGauge, 0, false)
			st.addValue(dp.GetValue(), dp.GetTimeUnixNano())
		}
	case *protocol.Metric_Sum:
		for _, dp := range d.Sum.GetDataPoints() {
			a.touchWindow(dp.GetTimeUnixNano())
			st := a.seriesFor(scope, m, dp.GetAttributes(), kindSum,
				d.Sum.GetTemporality(), d.Sum.GetIsMonotonic())
			if d.Sum.GetTemporality() == protocol.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA {
				st.addDelta(dp.GetValue(), dp.GetTimeUnixNano())
			} else {
				st.setLast(dp.GetValue(), dp.GetTimeUnixNano())
			}
		}
	case *protocol.Metric_Histogram:
		for _, hp := range d.Histogram.GetDataPoints() {
			a.touchWindow(hp.GetTimeUnixNano())
			st := a.seriesFor(scope, m, hp.GetAttributes(), kindHistogram,
				d.Histogram.GetTemporality(), false)
			st.mergeHistogram(hp)
		}
	}
}

// Flush emits the aggregated results and resets the window.
func (a *Aggregator) Flush() []Result {
	if len(a.series) == 0 {
		return nil
	}
	out := make([]Result, 0, len(a.series))
	for _, key := range a.order {
		st := a.series[key]
		out = append(out, Result{Scope: st.scope, Metric: st.build()})
	}
	a.series = make(map[string]*seriesStats)
	a.order = nil
	a.windowStart = time.Time{}
	return out
}

// seriesFor returns (creating on first sight) the accumulator for a series.
func (a *Aggregator) seriesFor(scope string, m *protocol.Metric, attrs map[string]string,
	kind metricKind, temporality protocol.AggregationTemporality, monotonic bool) *seriesStats {

	key := seriesKey(scope, m.GetName(), attrs, kind)
	st, ok := a.series[key]
	if !ok {
		st = &seriesStats{
			scope: scope, name: m.GetName(), unit: m.GetUnit(), desc: m.GetDescription(),
			attrs: attrs, kind: kind, temporality: temporality, monotonic: monotonic,
		}
		a.series[key] = st
		a.order = append(a.order, key)
	}
	return st
}

func (a *Aggregator) touchWindow(ts int64) {
	if a.windowStart.IsZero() {
		a.windowStart = time.Unix(0, ts)
	}
}

// addValue accumulates a gauge sample (min/mean/max/count over the window).
func (s *seriesStats) addValue(v float64, ts int64) {
	if s.count == 0 {
		s.min, s.max = v, v
		s.firstTS = ts
	} else {
		if v < s.min {
			s.min = v
		}
		if v > s.max {
			s.max = v
		}
	}
	s.count++
	s.sum += v
	s.lastTS = ts
}

// addDelta accumulates a delta sum: the window total is itself a delta.
func (s *seriesStats) addDelta(v float64, ts int64) {
	if s.count == 0 {
		s.firstTS = ts
	}
	s.count++
	s.deltaTotal += v
	s.lastTS = ts
}

// setLast keeps only the latest value of a cumulative counter — dropping
// intermediate points IS the downsampling for cumulative series.
func (s *seriesStats) setLast(v float64, ts int64) {
	if s.count == 0 {
		s.firstTS = ts
	}
	s.count++
	s.lastValue = v
	s.lastTS = ts
}

// mergeHistogram merges a histogram point bucket-wise. Bounds must match;
// on mismatch the series restarts its histogram accumulation (bounds are
// static in practice — the eBPF plugin hardcodes them).
func (s *seriesStats) mergeHistogram(hp *protocol.HistogramDataPoint) {
	if s.count == 0 || !sameBounds(s.bounds, hp.GetExplicitBounds()) {
		s.bounds = hp.GetExplicitBounds()
		s.bucketCounts = append([]uint64(nil), hp.GetBucketCounts()...)
		s.firstTS = hp.GetTimeUnixNano()
		s.min = hp.GetMin()
		s.max = hp.GetMax()
		s.count = hp.GetCount()
		s.sum = hp.GetSum()
		s.lastTS = hp.GetTimeUnixNano()
		return
	}
	for i, c := range hp.GetBucketCounts() {
		if i < len(s.bucketCounts) {
			s.bucketCounts[i] += c
		}
	}
	s.count += hp.GetCount()
	s.sum += hp.GetSum()
	if hp.GetMax() > s.max {
		s.max = hp.GetMax()
	}
	// min only shrinks when the incoming point actually reports one
	if hp.GetMin() != 0 && (s.min == 0 || hp.GetMin() < s.min) {
		s.min = hp.GetMin()
	}
	s.lastTS = hp.GetTimeUnixNano()
}

// build produces the aggregated single-point metric.
func (s *seriesStats) build() *protocol.Metric {
	m := &protocol.Metric{
		Name:        s.name,
		Unit:        s.unit,
		Description: s.desc,
	}

	switch s.kind {
	case kindSum:
		dp := &protocol.NumberDataPoint{
			Attributes:   s.attrs,
			TimeUnixNano: s.firstTS,
		}
		sum := &protocol.Sum{
			Temporality: s.temporality,
			IsMonotonic: s.monotonic,
		}
		if s.temporality == protocol.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA {
			dp.Value = s.deltaTotal
		} else {
			dp.Value = s.lastValue
			dp.TimeUnixNano = s.lastTS
		}
		sum.DataPoints = []*protocol.NumberDataPoint{dp}
		m.Data = &protocol.Metric_Sum{Sum: sum}

	case kindHistogram:
		m.Data = &protocol.Metric_Histogram{Histogram: &protocol.Histogram{
			Temporality: s.temporality,
			DataPoints: []*protocol.HistogramDataPoint{{
				Attributes:     s.attrs,
				TimeUnixNano:   s.firstTS,
				Count:          s.count,
				Sum:            s.sum,
				BucketCounts:   s.bucketCounts,
				ExplicitBounds: s.bounds,
				Min:            s.min,
				Max:            s.max,
			}},
		}}

	default: // gauge → degenerate histogram carrying (min/mean/max/count)
		m.Data = &protocol.Metric_Histogram{Histogram: &protocol.Histogram{
			Temporality: protocol.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
			DataPoints: []*protocol.HistogramDataPoint{{
				Attributes:     s.attrs,
				TimeUnixNano:   s.firstTS,
				Count:          s.count,
				Sum:            s.sum, // mean = sum / count
				BucketCounts:   []uint64{s.count},
				ExplicitBounds: nil,
				Min:            s.min,
				Max:            s.max,
			}},
		}}
	}
	return m
}

func seriesKey(scope, name string, attrs map[string]string, kind metricKind) string {
	var sb strings.Builder
	sb.WriteString(scope)
	sb.WriteByte(0)
	sb.WriteString(name)
	sb.WriteByte(0)
	fmt.Fprintf(&sb, "%d", kind)
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sb.WriteByte(0)
		sb.WriteString(k)
		sb.WriteByte(1)
		sb.WriteString(attrs[k])
	}
	return sb.String()
}

func sameBounds(a, b []float64) bool {
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
