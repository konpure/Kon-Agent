package codec

import (
	"math"
	"testing"

	"github.com/konpure/Kon-Agent/pkg/protocol"
	"google.golang.org/protobuf/proto"
)

const testPeriodNanos = int64(5_000_000_000) // 5s collection period

// buildBatch builds a realistic batch: one cpu gauge series, one ebpf
// sum-delta series and one RTT histogram series, each with `points` points
// starting at base.
func buildBatch(base int64, points int) *protocol.ExportMetricsRequest {
	req := &protocol.ExportMetricsRequest{
		Resource: &protocol.Resource{
			AgentId:    "edge-agent-1",
			Attributes: map[string]string{"host.name": "test-host"},
		},
		ExportTimeUnixNano: base,
	}

	cpu := &protocol.Metric{
		Name: "cpu_usage_percent",
		Unit: "%",
		Data: &protocol.Metric_Gauge{Gauge: &protocol.Gauge{}},
	}
	pkts := &protocol.Metric{
		Name: "network_packets_total",
		Unit: "{packet}",
		Data: &protocol.Metric_Sum{Sum: &protocol.Sum{
			Temporality: protocol.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
			IsMonotonic: true,
		}},
	}
	rtt := &protocol.Metric{
		Name: "tcp_rtt_seconds",
		Unit: "s",
		Data: &protocol.Metric_Histogram{Histogram: &protocol.Histogram{
			Temporality: protocol.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
		}},
	}

	for i := 0; i < points; i++ {
		ts := base + int64(i)*testPeriodNanos
		cpu.GetGauge().DataPoints = append(cpu.GetGauge().DataPoints, &protocol.NumberDataPoint{
			Attributes:   map[string]string{"mode": "user"},
			TimeUnixNano: ts,
			Value:        43.2 + float64(i%7)*0.13, // slowly varying
		})
		pkts.GetSum().DataPoints = append(pkts.GetSum().DataPoints, &protocol.NumberDataPoint{
			Attributes:   map[string]string{"interface": "eth0"},
			TimeUnixNano: ts,
			Value:        float64(1200 + i*37),
		})
		rtt.GetHistogram().DataPoints = append(rtt.GetHistogram().DataPoints, &protocol.HistogramDataPoint{
			Attributes:     map[string]string{},
			TimeUnixNano:   ts,
			Count:          uint64(100 + i),
			Sum:            0.42 + float64(i)*0.01,
			BucketCounts:   []uint64{10, 20, 30, 20, 10, 5, 3, 1, 1, 0, 0},
			ExplicitBounds: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5},
		})
	}

	req.ScopeMetrics = []*protocol.ScopeMetrics{
		{
			Scope:   &protocol.InstrumentationScope{Name: "cpu"},
			Metrics: []*protocol.Metric{cpu},
		},
		{
			Scope:   &protocol.InstrumentationScope{Name: "ebpf"},
			Metrics: []*protocol.Metric{pkts, rtt},
		},
	}
	return req
}

// flattenMap indexes a request's series points by series key for
// order-insensitive comparison.
func flattenMap(req *protocol.ExportMetricsRequest) map[string][]streamPoint {
	m := make(map[string][]streamPoint)
	for _, s := range flatten(req) {
		m[s.key] = s.points
	}
	return m
}

func assertSamePoints(t *testing.T, want, got *protocol.ExportMetricsRequest) {
	t.Helper()

	if want.GetResource().GetAgentId() != got.GetResource().GetAgentId() {
		t.Fatalf("agent id mismatch: %q vs %q", want.GetResource().GetAgentId(), got.GetResource().GetAgentId())
	}
	if want.GetResource().GetAttributes()["host.name"] != got.GetResource().GetAttributes()["host.name"] {
		t.Fatalf("resource attributes mismatch: %v vs %v", want.GetResource().GetAttributes(), got.GetResource().GetAttributes())
	}
	if want.GetExportTimeUnixNano() != got.GetExportTimeUnixNano() {
		t.Fatalf("export time mismatch: %d vs %d", want.GetExportTimeUnixNano(), got.GetExportTimeUnixNano())
	}

	wm, gm := flattenMap(want), flattenMap(got)
	if len(wm) != len(gm) {
		t.Fatalf("series count mismatch: %d vs %d", len(wm), len(gm))
	}
	for key, wpts := range wm {
		gpts, ok := gm[key]
		if !ok {
			t.Fatalf("series %q missing after decode", key)
		}
		if len(wpts) != len(gpts) {
			t.Fatalf("series %q point count mismatch: %d vs %d", key, len(wpts), len(gpts))
		}
		for i := range wpts {
			w, g := wpts[i], gpts[i]
			if w.ts != g.ts {
				t.Fatalf("series %q point %d ts mismatch: %d vs %d", key, i, w.ts, g.ts)
			}
			if w.hist == nil && math.Float64bits(w.val) != math.Float64bits(g.val) {
				t.Fatalf("series %q point %d value mismatch: %v vs %v", key, i, w.val, g.val)
			}
			if (w.hist == nil) != (g.hist == nil) {
				t.Fatalf("series %q point %d histogram presence mismatch", key, i)
			}
			if w.hist != nil {
				wh, gh := w.hist, g.hist
				if wh.GetCount() != gh.GetCount() ||
					math.Float64bits(wh.GetSum()) != math.Float64bits(gh.GetSum()) ||
					len(wh.GetBucketCounts()) != len(gh.GetBucketCounts()) ||
					len(wh.GetExplicitBounds()) != len(gh.GetExplicitBounds()) {
					t.Fatalf("series %q point %d histogram mismatch", key, i)
				}
				for j := range wh.GetBucketCounts() {
					if wh.GetBucketCounts()[j] != gh.GetBucketCounts()[j] {
						t.Fatalf("series %q point %d bucket %d mismatch", key, i, j)
					}
				}
				for j := range wh.GetExplicitBounds() {
					if wh.GetExplicitBounds()[j] != gh.GetExplicitBounds()[j] {
						t.Fatalf("series %q point %d bound %d mismatch", key, i, j)
					}
				}
			}
		}
	}
}

// TestStreamRoundTripMultiBatch encodes several consecutive batches with one
// encoder and decodes them with one decoder — the steady-state case where
// every frame after the first rides delta-of-delta and XOR streams.
func TestStreamRoundTripMultiBatch(t *testing.T) {
	enc := NewStreamEncoder()
	dec := NewStreamDecoder()

	base := int64(1_700_000_000_000_000_000)
	for batch := 0; batch < 6; batch++ {
		want := buildBatch(base+int64(batch)*2*testPeriodNanos, 2)
		flags, payload, err := enc.Encode(want)
		if err != nil {
			t.Fatalf("batch %d encode: %v", batch, err)
		}
		if flags != FlagGorilla {
			t.Fatalf("batch %d: expected gorilla flags, got %d", batch, flags)
		}
		got, err := dec.Decode(flags, payload)
		if err != nil {
			t.Fatalf("batch %d decode: %v", batch, err)
		}
		assertSamePoints(t, want, got)
	}
}

// TestMissingKeyframeDetected: a decoder that never saw the keyframe must
// reject a delta frame instead of producing garbage. The integration layer
// uses this error to reset the connection and resync.
func TestMissingKeyframeDetected(t *testing.T) {
	enc := NewStreamEncoder()
	base := int64(1_700_000_000_000_000_000)

	if _, _, err := enc.Encode(buildBatch(base, 2)); err != nil {
		t.Fatalf("encode batch 1: %v", err)
	}
	_, payload2, err := enc.Encode(buildBatch(base+2*testPeriodNanos, 2))
	if err != nil {
		t.Fatalf("encode batch 2: %v", err)
	}

	dec := NewStreamDecoder()
	if _, err := dec.Decode(FlagGorilla, payload2); err == nil {
		t.Fatal("expected missing-keyframe error, got nil")
	}
}

// TestStatelessModes covers raw proto and snappy-only round trips.
func TestStatelessModes(t *testing.T) {
	want := buildBatch(1_700_000_000_000_000_000, 3)
	for _, mode := range []Mode{ModeRaw, ModeSnappy} {
		flags, payload, err := Encode(want, mode)
		if err != nil {
			t.Fatalf("mode %d encode: %v", mode, err)
		}
		got, err := Decode(flags, payload)
		if err != nil {
			t.Fatalf("mode %d decode: %v", mode, err)
		}
		assertSamePoints(t, want, got)
	}
}

// TestSizeComparison is the preview of the paper's transport experiment:
// raw protobuf vs protobuf+snappy vs gorilla+snappy on identical data.
// Steady-state gorilla frames (post-keyframe) should beat raw proto clearly.
func TestSizeComparison(t *testing.T) {
	req := buildBatch(1_700_000_000_000_000_000, 12)

	_, raw, err := Encode(req, ModeRaw)
	if err != nil {
		t.Fatal(err)
	}
	_, snp, err := Encode(req, ModeSnappy)
	if err != nil {
		t.Fatal(err)
	}

	enc := NewStreamEncoder()
	if _, _, err := enc.Encode(buildBatch(1_700_000_000_000_000_000-testPeriodNanos*12, 12)); err != nil {
		t.Fatal(err) // warm-up batch carries the keyframes
	}
	_, gor, err := enc.Encode(req)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("wire size: raw proto = %d B, proto+snappy = %d B, gorilla+snappy = %d B",
		len(raw), len(snp), len(gor))

	if len(gor) >= len(raw) {
		t.Fatalf("gorilla steady-state frame (%d B) should beat raw proto (%d B)", len(gor), len(raw))
	}
}

// TestProtoBaseline guards against a trivial regression: the raw mode must
// produce exactly proto.Marshal output.
func TestProtoBaseline(t *testing.T) {
	req := buildBatch(1_700_000_000_000_000_000, 2)
	_, payload, err := Encode(req, ModeRaw)
	if err != nil {
		t.Fatal(err)
	}
	var back protocol.ExportMetricsRequest
	if err := proto.Unmarshal(payload, &back); err != nil {
		t.Fatal(err)
	}
	assertSamePoints(t, req, &back)
}
