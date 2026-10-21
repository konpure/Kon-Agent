package aggregate

import (
	"math"
	"testing"
	"time"

	"github.com/konpure/Kon-Agent/pkg/protocol"
)

const testBase = int64(1_700_000_000_000_000_000)

func gaugeMetric(name string, attrs map[string]string, points ...struct {
	ts  int64
	val float64
}) *protocol.Metric {
	m := &protocol.Metric{
		Name: name,
		Unit: "%",
		Data: &protocol.Metric_Gauge{Gauge: &protocol.Gauge{}},
	}
	for _, p := range points {
		m.GetGauge().DataPoints = append(m.GetGauge().DataPoints, &protocol.NumberDataPoint{
			Attributes:   attrs,
			TimeUnixNano: p.ts,
			Value:        p.val,
		})
	}
	return m
}

func TestGaugeAggregatesToHistogram(t *testing.T) {
	agg := New(30 * time.Second)

	// 6 points at 5s intervals: values 10, 20, 30, 40, 50, 60
	vals := []float64{10, 20, 30, 40, 50, 60}
	for i, v := range vals {
		m := gaugeMetric("cpu_usage_percent", map[string]string{"mode": "user"})
		m.GetGauge().DataPoints = []*protocol.NumberDataPoint{{
			Attributes:   map[string]string{"mode": "user"},
			TimeUnixNano: testBase + int64(i)*5_000_000_000,
			Value:        v,
		}}
		agg.Add("cpu", m)
	}

	if agg.Empty() {
		t.Fatal("aggregator should not be empty")
	}
	if !agg.Due(time.Unix(0, testBase).Add(31 * time.Second)) {
		t.Fatal("window should be due after 31s")
	}

	results := agg.Flush()
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !agg.Empty() {
		t.Fatal("aggregator should be empty after flush")
	}

	r := results[0]
	if r.Scope != "cpu" {
		t.Fatalf("scope mismatch: %q", r.Scope)
	}
	h := r.Metric.GetHistogram()
	if h == nil {
		t.Fatal("gauge should aggregate into a histogram")
	}
	if h.GetTemporality() != protocol.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA {
		t.Fatal("aggregated histogram should be delta")
	}
	dp := h.GetDataPoints()[0]
	if dp.GetCount() != 6 {
		t.Fatalf("count mismatch: %d", dp.GetCount())
	}
	if dp.GetSum() != 210 {
		t.Fatalf("sum mismatch: %v", dp.GetSum())
	}
	if got := dp.GetSum() / float64(dp.GetCount()); got != 35 {
		t.Fatalf("mean mismatch: %v", got)
	}
	if dp.GetMin() != 10 || dp.GetMax() != 60 {
		t.Fatalf("min/max mismatch: %v/%v", dp.GetMin(), dp.GetMax())
	}
	if dp.GetTimeUnixNano() != testBase {
		t.Fatalf("timestamp should be window start: %d", dp.GetTimeUnixNano())
	}
}

func TestSumDeltaAccumulates(t *testing.T) {
	agg := New(30 * time.Second)

	for i := 0; i < 3; i++ {
		m := &protocol.Metric{
			Name: "network_packets_total",
			Data: &protocol.Metric_Sum{Sum: &protocol.Sum{
				Temporality: protocol.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
				IsMonotonic: true,
			}},
		}
		m.GetSum().DataPoints = []*protocol.NumberDataPoint{{
			Attributes:   map[string]string{"interface": "eth0"},
			TimeUnixNano: testBase + int64(i)*5_000_000_000,
			Value:        float64(100 * (i + 1)),
		}}
		agg.Add("ebpf", m)
	}

	results := agg.Flush()
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	sum := results[0].Metric.GetSum()
	if sum == nil {
		t.Fatal("delta sum should stay a sum")
	}
	if sum.GetTemporality() != protocol.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA {
		t.Fatal("window total should keep delta temporality")
	}
	if got := sum.GetDataPoints()[0].GetValue(); got != 600 {
		t.Fatalf("expected window total 600, got %v", got)
	}
	if !sum.GetIsMonotonic() {
		t.Fatal("monotonic flag lost")
	}
}

func TestSumCumulativeKeepsLatest(t *testing.T) {
	agg := New(30 * time.Second)

	for i := 0; i < 4; i++ {
		m := &protocol.Metric{
			Name: "disk_io_read_bytes_total",
			Data: &protocol.Metric_Sum{Sum: &protocol.Sum{
				Temporality: protocol.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
				IsMonotonic: true,
			}},
		}
		m.GetSum().DataPoints = []*protocol.NumberDataPoint{{
			Attributes:   map[string]string{"device": "sda"},
			TimeUnixNano: testBase + int64(i)*5_000_000_000,
			Value:        float64(1000 * (i + 1)),
		}}
		agg.Add("disk", m)
	}

	results := agg.Flush()
	sum := results[0].Metric.GetSum()
	if got := sum.GetDataPoints()[0].GetValue(); got != 4000 {
		t.Fatalf("cumulative should keep latest value 4000, got %v", got)
	}
	if sum.GetTemporality() != protocol.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
		t.Fatal("cumulative temporality lost")
	}
}

func TestHistogramMergesBucketWise(t *testing.T) {
	agg := New(30 * time.Second)
	bounds := []float64{0.001, 0.01, 0.1}

	for i := 0; i < 2; i++ {
		m := &protocol.Metric{
			Name: "tcp_rtt_seconds",
			Data: &protocol.Metric_Histogram{Histogram: &protocol.Histogram{
				Temporality: protocol.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
			}},
		}
		m.GetHistogram().DataPoints = []*protocol.HistogramDataPoint{{
			TimeUnixNano:   testBase + int64(i)*5_000_000_000,
			Count:          10,
			Sum:            0.5,
			BucketCounts:   []uint64{1, 2, 3, 4},
			ExplicitBounds: bounds,
		}}
		agg.Add("ebpf", m)
	}

	results := agg.Flush()
	h := results[0].Metric.GetHistogram()
	if h == nil {
		t.Fatal("histogram should stay a histogram")
	}
	dp := h.GetDataPoints()[0]
	if dp.GetCount() != 20 {
		t.Fatalf("count mismatch: %d", dp.GetCount())
	}
	if math.Abs(dp.GetSum()-1.0) > 1e-9 {
		t.Fatalf("sum mismatch: %v", dp.GetSum())
	}
	want := []uint64{2, 4, 6, 8}
	for i, c := range dp.GetBucketCounts() {
		if c != want[i] {
			t.Fatalf("bucket %d mismatch: %d vs %d", i, c, want[i])
		}
	}
	if len(dp.GetExplicitBounds()) != 3 {
		t.Fatal("bounds lost in merge")
	}
}

func TestSeriesSeparatedByAttributes(t *testing.T) {
	agg := New(30 * time.Second)

	for _, mode := range []string{"user", "system"} {
		m := gaugeMetric("cpu_usage_percent", nil)
		m.GetGauge().DataPoints = []*protocol.NumberDataPoint{{
			Attributes:   map[string]string{"mode": mode},
			TimeUnixNano: testBase,
			Value:        10,
		}}
		agg.Add("cpu", m)
	}

	results := agg.Flush()
	if len(results) != 2 {
		t.Fatalf("different attribute sets must form separate series, got %d", len(results))
	}
}

func TestFlushOnEmptyIsNil(t *testing.T) {
	agg := New(30 * time.Second)
	if agg.Flush() != nil {
		t.Fatal("flush on empty aggregator should return nil")
	}
	if agg.Due(time.Now()) {
		t.Fatal("empty aggregator should never be due")
	}
}
