package pipeline

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"time"

	"github.com/konpure/Kon-Agent/internal/transport/codec"
	"github.com/konpure/Kon-Agent/internal/transport/quic"
	"github.com/konpure/Kon-Agent/pkg/protocol"
	"google.golang.org/protobuf/proto"
)

// Exporter is the pipeline's sink stage. It encodes batches according to the
// configured codec mode and ships the frames over QUIC.
//
// Codec state management (the crucial invariant):
// The Gorilla encoder's per-series state is scoped to one QUIC connection.
// The server creates a fresh decoder per connection, so the encoder must
// reset whenever the connection is (re)established — otherwise it would emit
// delta frames the server cannot resolve.
//
// Retry semantics: a failed batch is NOT retried with the same encoded frame
// (the encoder state already advanced). Instead the connection is dropped,
// both sides reset, and the retry re-encodes from raw proto — the WAL keeps
// the record until acked, so nothing is lost.
type Exporter struct {
	client     *quic.Client
	maxRetries int
	mode       codec.Mode
	encoder    *codec.StreamEncoder
}

func NewExporter(client *quic.Client, maxRetries int, mode codec.Mode) *Exporter {
	return &Exporter{
		client:     client,
		maxRetries: maxRetries,
		mode:       mode,
		encoder:    codec.NewStreamEncoder(),
	}
}

// ExportBatch encodes raw proto bytes and ships the frame with reconnect and
// exponential backoff (plus jitter).
func (e *Exporter) ExportBatch(ctx context.Context, rawProto []byte) error {
	if len(rawProto) == 0 {
		return nil
	}

	var req protocol.ExportMetricsRequest
	if err := proto.Unmarshal(rawProto, &req); err != nil {
		return fmt.Errorf("failed to unmarshal raw proto: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= e.maxRetries; attempt++ {
		if attempt > 0 {
			sleepDuration := time.Duration(math.Pow(2, float64(attempt))) * 100 * time.Millisecond
			jitter := time.Duration(rand.Int63n(int64(sleepDuration)))
			sleepDuration += jitter

			select {
			case <-ctx.Done():
				return fmt.Errorf("context cancelled while retrying: %w", ctx.Err())
			case <-time.After(sleepDuration):
			}
		}

		if !e.client.IsConnected() {
			if err := e.client.Connect(ctx); err != nil {
				lastErr = fmt.Errorf("failed to connect to QUIC server: %w", err)
				continue
			}
			// Fresh connection: the server's decoder starts empty, so the
			// encoder must restart from keyframes too.
			e.encoder.Reset()
		}

		// Encode against the current connection state. This must happen
		// after any reconnect/reset so the frame matches the decoder.
		flags, wireData, err := e.encode(&req)
		if err != nil {
			return fmt.Errorf("failed to encode batch: %w", err)
		}

		if err := e.client.SendRaw(ctx, flags, wireData); err != nil {
			// A partial frame write corrupts the ordered stream; close the
			// connection so the next attempt reconnects and both sides
			// resync from keyframes.
			_ = e.client.Close()
			e.encoder.Reset()
			lastErr = fmt.Errorf("failed to send batch: %w", err)
			continue
		}
		return nil
	}
	return fmt.Errorf("failed to export batch after %d attempts: %w", e.maxRetries, lastErr)
}

func (e *Exporter) encode(req *protocol.ExportMetricsRequest) (uint8, []byte, error) {
	if e.mode == codec.ModeGorilla {
		return e.encoder.Encode(req)
	}
	return codec.Encode(req, e.mode)
}
