package codec

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	"github.com/konpure/Kon-Agent/pkg/protocol"
)

const (
	kindGauge     byte = 0
	kindSum       byte = 1
	kindHistogram byte = 2
)

// maxSeries bounds the encoder/decoder state maps, guarding against label
// explosion (the tag/field discipline: high-cardinality attributes are
// forbidden; this is the safety net).
const maxSeries = 100000

// streamPoint is one datapoint inside a series being encoded.
type streamPoint struct {
	ts   int64
	val  float64
	hist *protocol.HistogramDataPoint
}

type streamSeries struct {
	scope, scopeVer  string
	name, unit, desc string
	kind             byte
	temporality      protocol.AggregationTemporality
	monotonic        bool
	attrs            [][2]string
	key              string
	points           []streamPoint
}

func seriesKey(scope, name string, attrs [][2]string) string {
	var sb strings.Builder
	sb.WriteString(scope)
	sb.WriteByte(0)
	sb.WriteString(name)
	for _, kv := range attrs {
		sb.WriteByte(0)
		sb.WriteString(kv[0])
		sb.WriteByte(1)
		sb.WriteString(kv[1])
	}
	return sb.String()
}

func sortedAttrPairs(m map[string]string) [][2]string {
	pairs := make([][2]string, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, [2]string{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i][0] < pairs[j][0] })
	return pairs
}

func attrMap(pairs [][2]string) map[string]string {
	m := make(map[string]string, len(pairs))
	for _, kv := range pairs {
		m[kv[0]] = kv[1]
	}
	return m
}

// flatten explodes the three-layer request into per-attribute-set series,
// preserving first-seen order.
func flatten(req *protocol.ExportMetricsRequest) []*streamSeries {
	var out []*streamSeries
	idx := make(map[string]*streamSeries)
	push := func(s *streamSeries, p streamPoint) {
		if existing, ok := idx[s.key]; ok {
			existing.points = append(existing.points, p)
			return
		}
		s.points = []streamPoint{p}
		idx[s.key] = s
		out = append(out, s)
	}

	for _, sm := range req.GetScopeMetrics() {
		scopeName := sm.GetScope().GetName()
		scopeVer := sm.GetScope().GetVersion()
		for _, m := range sm.GetMetrics() {
			base := streamSeries{
				scope: scopeName, scopeVer: scopeVer,
				name: m.GetName(), unit: m.GetUnit(), desc: m.GetDescription(),
			}
			switch d := m.GetData().(type) {
			case *protocol.Metric_Gauge:
				for _, dp := range d.Gauge.GetDataPoints() {
					s := base
					s.kind = kindGauge
					s.attrs = sortedAttrPairs(dp.GetAttributes())
					s.key = seriesKey(s.scope, s.name, s.attrs)
					push(&s, streamPoint{ts: dp.GetTimeUnixNano(), val: dp.GetValue()})
				}
			case *protocol.Metric_Sum:
				for _, dp := range d.Sum.GetDataPoints() {
					s := base
					s.kind = kindSum
					s.temporality = d.Sum.GetTemporality()
					s.monotonic = d.Sum.GetIsMonotonic()
					s.attrs = sortedAttrPairs(dp.GetAttributes())
					s.key = seriesKey(s.scope, s.name, s.attrs)
					push(&s, streamPoint{ts: dp.GetTimeUnixNano(), val: dp.GetValue()})
				}
			case *protocol.Metric_Histogram:
				for _, dp := range d.Histogram.GetDataPoints() {
					s := base
					s.kind = kindHistogram
					s.temporality = d.Histogram.GetTemporality()
					s.attrs = sortedAttrPairs(dp.GetAttributes())
					s.key = seriesKey(s.scope, s.name, s.attrs)
					push(&s, streamPoint{ts: dp.GetTimeUnixNano(), hist: dp})
				}
			}
		}
	}
	return out
}

// StreamEncoder compresses batches as one continuing stream. Its series state
// is scoped to a single QUIC connection and reset on reconnect.
type StreamEncoder struct {
	series map[string]*seriesState
}

func NewStreamEncoder() *StreamEncoder {
	return &StreamEncoder{series: make(map[string]*seriesState)}
}

// Reset drops all series state; the next frame per series is a keyframe.
func (e *StreamEncoder) Reset() {
	e.series = make(map[string]*seriesState)
}

func (e *StreamEncoder) state(key string) *seriesState {
	st, ok := e.series[key]
	if !ok {
		st = &seriesState{}
		e.series[key] = st
	}
	return st
}

// EncodeFrame compresses one batch, continuing per-series stream state.
//
// Frame layout:
//
//	[dictionary]
//	[uvarint agentRef][uvarint exportTime][uvarint attrCount][(k,v) refs]
//	[uvarint seriesCount]
//	per series: refs | kind | [sum flags] | attrs | pointCount | points
func (e *StreamEncoder) EncodeFrame(req *protocol.ExportMetricsRequest) ([]byte, error) {
	series := flatten(req)

	dict := newDictionary()
	res := req.GetResource()
	agentRef := dict.ref(res.GetAgentId())
	resAttrs := sortedAttrPairs(res.GetAttributes())
	for _, kv := range resAttrs {
		dict.ref(kv[0])
		dict.ref(kv[1])
	}
	for _, s := range series {
		dict.ref(s.scope)
		dict.ref(s.scopeVer)
		dict.ref(s.name)
		dict.ref(s.unit)
		dict.ref(s.desc)
		for _, kv := range s.attrs {
			dict.ref(kv[0])
			dict.ref(kv[1])
		}
	}

	buf := &bytes.Buffer{}
	dict.writeTo(buf)
	writeUvarint(buf, uint64(agentRef))
	writeUvarint(buf, uint64(req.GetExportTimeUnixNano()))
	writeUvarint(buf, uint64(len(resAttrs)))
	for _, kv := range resAttrs {
		writeUvarint(buf, uint64(dict.ref(kv[0])))
		writeUvarint(buf, uint64(dict.ref(kv[1])))
	}

	writeUvarint(buf, uint64(len(series)))
	for _, s := range series {
		writeUvarint(buf, uint64(dict.ref(s.scope)))
		writeUvarint(buf, uint64(dict.ref(s.scopeVer)))
		writeUvarint(buf, uint64(dict.ref(s.name)))
		writeUvarint(buf, uint64(dict.ref(s.unit)))
		writeUvarint(buf, uint64(dict.ref(s.desc)))
		buf.WriteByte(s.kind)
		if s.kind == kindSum {
			var f byte
			if s.monotonic {
				f = 1
			}
			buf.WriteByte(byte(s.temporality)<<1 | f)
		}
		writeUvarint(buf, uint64(len(s.attrs)))
		for _, kv := range s.attrs {
			writeUvarint(buf, uint64(dict.ref(kv[0])))
			writeUvarint(buf, uint64(dict.ref(kv[1])))
		}

		if s.kind == kindHistogram {
			e.encodeHistogramSeries(buf, s)
		} else {
			e.encodeNumberSeries(buf, s)
		}
	}
	return buf.Bytes(), nil
}

// encodeNumberSeries writes [pointCount][rawFlag][raw point?][ts bitstream][val bitstream].
// The first-ever point of a series is a raw keyframe; later points ride the
// delta-of-delta and XOR streams.
func (e *StreamEncoder) encodeNumberSeries(buf *bytes.Buffer, s *streamSeries) {
	// Beyond the series cap, encode statelessly (every point raw) to bound memory.
	stateful := len(e.series) < maxSeries
	var st *seriesState
	if stateful {
		st = e.state(s.key)
	} else {
		st = &seriesState{}
	}

	writeUvarint(buf, uint64(len(s.points)))
	pts := s.points
	if !st.initialized {
		buf.WriteByte(1) // keyframe point
		p := pts[0]
		writeUvarint(buf, zigzag(p.ts))
		writeFloat64(buf, p.val)
		// lastDelta starts at 0, so the first point after a keyframe carries
		// the whole collection period as its dod (5s = 5e9 ns in UnixNano).
		// writeDod's 64-bit tier is what keeps that from being truncated.
		st.lastTS, st.lastDelta, st.lastVal = p.ts, 0, math.Float64bits(p.val)
		st.hasWindow = false
		st.initialized = true
		pts = pts[1:]
	} else {
		buf.WriteByte(0)
	}

	tsW, valW := &bitWriter{}, &bitWriter{}
	for _, p := range pts {
		delta := p.ts - st.lastTS
		writeDod(tsW, delta-st.lastDelta)
		st.lastDelta, st.lastTS = delta, p.ts
		writeValueXOR(valW, st, p.val)
	}
	tsBytes, valBytes := tsW.bytes(), valW.bytes()
	writeUvarint(buf, uint64(len(tsBytes)))
	buf.Write(tsBytes)
	writeUvarint(buf, uint64(len(valBytes)))
	buf.Write(valBytes)
}

// encodeHistogramSeries writes [pointCount][per point: rawFlag + fields].
// Bucket bounds are stored once (keyframe) and inherited by delta points;
// counts and sum are plain varints/floats — histogram points are rare and
// small, the win is in the number streams.
func (e *StreamEncoder) encodeHistogramSeries(buf *bytes.Buffer, s *streamSeries) {
	st := e.state(s.key)
	writeUvarint(buf, uint64(len(s.points)))
	for _, p := range s.points {
		hp := p.hist
		if !st.initialized {
			buf.WriteByte(1)
			writeUvarint(buf, zigzag(p.ts))
			writeUvarint(buf, hp.GetCount())
			writeFloat64(buf, hp.GetSum())
			writeUvarint(buf, uint64(len(hp.GetExplicitBounds())))
			for _, b := range hp.GetExplicitBounds() {
				writeFloat64(buf, b)
			}
			st.histBounds = hp.GetExplicitBounds()
			st.initialized = true
		} else {
			buf.WriteByte(0)
			writeUvarint(buf, zigzag(p.ts-st.lastTS))
			writeUvarint(buf, hp.GetCount())
			writeFloat64(buf, hp.GetSum())
		}
		st.lastTS = p.ts
		writeUvarint(buf, uint64(len(hp.GetBucketCounts())))
		for _, c := range hp.GetBucketCounts() {
			writeUvarint(buf, c)
		}
	}
}

// StreamDecoder applies frames to a mirror of the encoder's state.
type StreamDecoder struct {
	series map[string]*seriesState
}

func NewStreamDecoder() *StreamDecoder {
	return &StreamDecoder{series: make(map[string]*seriesState)}
}

// DecodeFrame decodes one frame and rebuilds the three-layer request.
// A delta frame for a series never seen on this connection is an error —
// the integration layer reacts by resetting the connection, which forces
// keyframes and resynchronizes both sides.
func (d *StreamDecoder) DecodeFrame(frame []byte) (*protocol.ExportMetricsRequest, error) {
	r := bytes.NewReader(frame)
	strs, err := readDictionary(r)
	if err != nil {
		return nil, err
	}
	ref := func() (string, error) {
		i, err := binary.ReadUvarint(r)
		if err != nil {
			return "", err
		}
		if i >= uint64(len(strs)) {
			return "", fmt.Errorf("codec: dict ref %d out of range", i)
		}
		return strs[i], nil
	}
	readPairs := func() ([][2]string, error) {
		n, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, err
		}
		if n > 1<<10 {
			return nil, fmt.Errorf("codec: absurd attribute count %d", n)
		}
		pairs := make([][2]string, 0, n)
		for i := uint64(0); i < n; i++ {
			k, err := ref()
			if err != nil {
				return nil, err
			}
			v, err := ref()
			if err != nil {
				return nil, err
			}
			pairs = append(pairs, [2]string{k, v})
		}
		return pairs, nil
	}

	agentID, err := ref()
	if err != nil {
		return nil, err
	}
	exportTime, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, err
	}
	resPairs, err := readPairs()
	if err != nil {
		return nil, err
	}

	req := &protocol.ExportMetricsRequest{
		Resource: &protocol.Resource{
			AgentId:    agentID,
			Attributes: attrMap(resPairs),
		},
		ExportTimeUnixNano: int64(exportTime),
	}

	seriesCount, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, err
	}
	if seriesCount > 1<<20 {
		return nil, fmt.Errorf("codec: absurd series count %d", seriesCount)
	}

	scopes := make(map[string]*protocol.ScopeMetrics)
	metrics := make(map[string]*protocol.Metric)
	var scopeOrder []string

	for i := uint64(0); i < seriesCount; i++ {
		scope, err := ref()
		if err != nil {
			return nil, err
		}
		scopeVer, err := ref()
		if err != nil {
			return nil, err
		}
		name, err := ref()
		if err != nil {
			return nil, err
		}
		unit, err := ref()
		if err != nil {
			return nil, err
		}
		desc, err := ref()
		if err != nil {
			return nil, err
		}
		kind, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		var temporality protocol.AggregationTemporality
		var monotonic bool
		if kind == kindSum {
			f, err := r.ReadByte()
			if err != nil {
				return nil, err
			}
			temporality = protocol.AggregationTemporality(f >> 1)
			monotonic = f&1 == 1
		}
		attrs, err := readPairs()
		if err != nil {
			return nil, err
		}
		key := seriesKey(scope, name, attrs)
		st := d.series[key]

		var points []streamPoint
		if kind == kindHistogram {
			points, st, err = d.decodeHistogramSeries(r, st)
		} else {
			points, st, err = d.decodeNumberSeries(r, st)
		}
		if err != nil {
			return nil, err
		}
		d.series[key] = st

		// Rebuild the three-layer structure.
		sm, ok := scopes[scope]
		if !ok {
			sm = &protocol.ScopeMetrics{
				Scope: &protocol.InstrumentationScope{Name: scope, Version: scopeVer},
			}
			scopes[scope] = sm
			scopeOrder = append(scopeOrder, scope)
		}
		mkey := scope + "\x00" + name
		m, ok := metrics[mkey]
		if !ok {
			m = &protocol.Metric{Name: name, Unit: unit, Description: desc}
			switch kind {
			case kindSum:
				m.Data = &protocol.Metric_Sum{Sum: &protocol.Sum{
					Temporality: temporality,
					IsMonotonic: monotonic,
				}}
			case kindHistogram:
				m.Data = &protocol.Metric_Histogram{Histogram: &protocol.Histogram{
					Temporality: temporality,
				}}
			default:
				m.Data = &protocol.Metric_Gauge{Gauge: &protocol.Gauge{}}
			}
			metrics[mkey] = m
			sm.Metrics = append(sm.Metrics, m)
		}

		am := attrMap(attrs)
		for _, p := range points {
			switch kind {
			case kindHistogram:
				m.GetHistogram().DataPoints = append(m.GetHistogram().DataPoints, p.hist)
			case kindSum:
				m.GetSum().DataPoints = append(m.GetSum().DataPoints, &protocol.NumberDataPoint{
					Attributes:   am,
					TimeUnixNano: p.ts,
					Value:        p.val,
				})
			default:
				m.GetGauge().DataPoints = append(m.GetGauge().DataPoints, &protocol.NumberDataPoint{
					Attributes:   am,
					TimeUnixNano: p.ts,
					Value:        p.val,
				})
			}
		}
	}

	for _, name := range scopeOrder {
		req.ScopeMetrics = append(req.ScopeMetrics, scopes[name])
	}
	return req, nil
}

func (d *StreamDecoder) decodeNumberSeries(r *bytes.Reader, st *seriesState) ([]streamPoint, *seriesState, error) {
	count, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, nil, err
	}
	rawFlag, err := r.ReadByte()
	if err != nil {
		return nil, nil, err
	}
	if st == nil {
		if rawFlag == 0 {
			return nil, nil, fmt.Errorf("codec: delta frame for unknown series (missing keyframe)")
		}
		st = &seriesState{}
	}
	// Decode against a scratch copy: the caller drops the whole frame on
	// error, and a mid-decode failure must not poison the committed state
	// (st points into d.series). Only success commits via the returned pointer.
	work := *st

	pts := make([]streamPoint, 0, count)
	start := uint64(0)
	if rawFlag == 1 {
		tsu, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, nil, err
		}
		val, err := readFloat64(r)
		if err != nil {
			return nil, nil, err
		}
		ts := unzigzag(tsu)
		work.lastTS, work.lastDelta, work.lastVal = ts, 0, math.Float64bits(val)
		work.hasWindow = false
		work.initialized = true
		pts = append(pts, streamPoint{ts: ts, val: val})
		start = 1
	}

	tsLen, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, nil, err
	}
	tsBytes := make([]byte, tsLen)
	if _, err := io.ReadFull(r, tsBytes); err != nil {
		return nil, nil, err
	}
	valLen, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, nil, err
	}
	valBytes := make([]byte, valLen)
	if _, err := io.ReadFull(r, valBytes); err != nil {
		return nil, nil, err
	}

	tsR := &bitReader{data: tsBytes}
	valR := &bitReader{data: valBytes}
	for i := start; i < count; i++ {
		dod, err := readDod(tsR)
		if err != nil {
			return nil, nil, err
		}
		work.lastDelta += dod
		work.lastTS += work.lastDelta
		val, err := readValueXOR(valR, &work)
		if err != nil {
			return nil, nil, err
		}
		pts = append(pts, streamPoint{ts: work.lastTS, val: val})
	}
	return pts, &work, nil
}

func (d *StreamDecoder) decodeHistogramSeries(r *bytes.Reader, st *seriesState) ([]streamPoint, *seriesState, error) {
	count, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, nil, err
	}
	if count > 1<<16 {
		return nil, nil, fmt.Errorf("codec: absurd histogram point count %d", count)
	}
	if st == nil {
		st = &seriesState{}
	}
	// Scratch copy, same anti-poisoning rationale as decodeNumberSeries.
	work := *st

	pts := make([]streamPoint, 0, count)
	for i := uint64(0); i < count; i++ {
		rawFlag, err := r.ReadByte()
		if err != nil {
			return nil, nil, err
		}
		hp := &protocol.HistogramDataPoint{}
		if rawFlag == 1 {
			tsu, err := binary.ReadUvarint(r)
			if err != nil {
				return nil, nil, err
			}
			work.lastTS = unzigzag(tsu)
			c, err := binary.ReadUvarint(r)
			if err != nil {
				return nil, nil, err
			}
			hp.Count = c
			sum, err := readFloat64(r)
			if err != nil {
				return nil, nil, err
			}
			hp.Sum = sum
			nb, err := binary.ReadUvarint(r)
			if err != nil {
				return nil, nil, err
			}
			if nb > 1<<10 {
				return nil, nil, fmt.Errorf("codec: absurd bound count %d", nb)
			}
			bounds := make([]float64, 0, nb)
			for j := uint64(0); j < nb; j++ {
				b, err := readFloat64(r)
				if err != nil {
					return nil, nil, err
				}
				bounds = append(bounds, b)
			}
			hp.ExplicitBounds = bounds
			work.histBounds = bounds
			work.initialized = true
		} else {
			if !work.initialized {
				return nil, nil, fmt.Errorf("codec: histogram delta frame for unknown series")
			}
			dts, err := binary.ReadUvarint(r)
			if err != nil {
				return nil, nil, err
			}
			work.lastTS += unzigzag(dts)
			c, err := binary.ReadUvarint(r)
			if err != nil {
				return nil, nil, err
			}
			hp.Count = c
			sum, err := readFloat64(r)
			if err != nil {
				return nil, nil, err
			}
			hp.Sum = sum
			hp.ExplicitBounds = work.histBounds
		}
		hp.TimeUnixNano = work.lastTS

		nb, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, nil, err
		}
		if nb > 1<<10 {
			return nil, nil, fmt.Errorf("codec: absurd bucket count %d", nb)
		}
		buckets := make([]uint64, 0, nb)
		for j := uint64(0); j < nb; j++ {
			c, err := binary.ReadUvarint(r)
			if err != nil {
				return nil, nil, err
			}
			buckets = append(buckets, c)
		}
		hp.BucketCounts = buckets
		pts = append(pts, streamPoint{ts: work.lastTS, hist: hp})
	}
	return pts, &work, nil
}
