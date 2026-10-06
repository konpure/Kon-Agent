package pipeline

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"time"

	"github.com/konpure/Kon-Agent/internal/transport/quic"
)

// Exporter is the pipeline's sink stage: it ships pre-marshaled batches over
// QUIC with reconnect and exponential backoff (plus jitter).
//
// Retry semantics: a limited number of attempts is made and the error is
// returned to the caller. With WAL enabled the caller is the WAL consumer,
// which keeps retrying indefinitely — the data is durable, so there is no
// "give up and drop" path anymore.
type Exporter struct {
	client     *quic.Client
	maxRetries int
}

func NewExporter(client *quic.Client, maxRetries int) *Exporter {
	return &Exporter{
		client:     client,
		maxRetries: maxRetries,
	}
}

// ExportBytes ships a pre-marshaled batch (length-prefixed on the wire).
func (e *Exporter) ExportBytes(ctx context.Context, payload []byte) error {
	if len(payload) == 0 {
		return nil
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
		}

		if err := e.client.SendRaw(ctx, payload); err != nil {
			lastErr = fmt.Errorf("failed to send batch: %w", err)
			continue
		}
		return nil
	}
	return fmt.Errorf("failed to send batch after %d attempts: %w", e.maxRetries, lastErr)
}
