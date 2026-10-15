// Package codec implements the wire encoding of metric batches.
//
// Three encodings are supported, selected by a 1-byte flags field that is
// carried both in the WAL record header and in the wire frame header:
//
//	FlagRawProto: payload = proto.Marshal(ExportMetricsRequest)
//	FlagSnappy:   payload = snappy(proto)            — Prometheus Remote Write style
//	FlagGorilla:  payload = snappy(gorilla frame)    — delta-of-delta timestamps,
//	              XOR values, per-frame string dictionary (Gorilla + VictoriaMetrics)
//
// The three modes are exactly the arms of the paper's transport experiment:
// raw protobuf vs protobuf+snappy vs Gorilla-style encoding+snappy.
package codec

import (
	"fmt"

	"github.com/golang/snappy"
	"github.com/konpure/Kon-Agent/pkg/protocol"
	"google.golang.org/protobuf/proto"
)

// Frame flags stored in the WAL record header and sent in the wire frame
// header: [length:4][flags:1][payload].
const (
	FlagRawProto uint8 = 0
	FlagSnappy   uint8 = 1
	FlagGorilla  uint8 = 2
)

// Mode selects the encoding used by the agent.
type Mode int

const (
	ModeRaw Mode = iota
	ModeSnappy
	ModeGorilla
)

// Encode produces a self-contained payload for the stateless modes.
// ModeGorilla is stateful and requires a StreamEncoder.
func Encode(req *protocol.ExportMetricsRequest, mode Mode) (uint8, []byte, error) {
	switch mode {
	case ModeRaw:
		payload, err := proto.Marshal(req)
		return FlagRawProto, payload, err
	case ModeSnappy:
		raw, err := proto.Marshal(req)
		if err != nil {
			return 0, nil, err
		}
		return FlagSnappy, snappy.Encode(nil, raw), nil
	default:
		return 0, nil, fmt.Errorf("codec: mode %d requires StreamEncoder", mode)
	}
}

// Decode reverses Encode for the stateless modes.
func Decode(flags uint8, payload []byte) (*protocol.ExportMetricsRequest, error) {
	switch flags {
	case FlagRawProto:
		var req protocol.ExportMetricsRequest
		if err := proto.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("codec: unmarshal raw proto: %w", err)
		}
		return &req, nil
	case FlagSnappy:
		raw, err := snappy.Decode(nil, payload)
		if err != nil {
			return nil, fmt.Errorf("codec: snappy decode: %w", err)
		}
		var req protocol.ExportMetricsRequest
		if err := proto.Unmarshal(raw, &req); err != nil {
			return nil, fmt.Errorf("codec: unmarshal snappy proto: %w", err)
		}
		return &req, nil
	default:
		return nil, fmt.Errorf("codec: flags %d require StreamDecoder", flags)
	}
}

// Encode compresses one batch continuing the stream state, then snappy.
func (e *StreamEncoder) Encode(req *protocol.ExportMetricsRequest) (uint8, []byte, error) {
	frame, err := e.EncodeFrame(req)
	if err != nil {
		return 0, nil, err
	}
	return FlagGorilla, snappy.Encode(nil, frame), nil
}

// Decode applies one frame to the decoder state. Stateless flags are handled
// without touching the stream state.
func (d *StreamDecoder) Decode(flags uint8, payload []byte) (*protocol.ExportMetricsRequest, error) {
	if flags != FlagGorilla {
		return Decode(flags, payload)
	}
	frame, err := snappy.Decode(nil, payload)
	if err != nil {
		return nil, fmt.Errorf("codec: snappy decode: %w", err)
	}
	return d.DecodeFrame(frame)
}
