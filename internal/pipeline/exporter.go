package pipeline

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"time"

	"github.com/konpure/Kon-Agent/internal/transport/quic"
	"github.com/konpure/Kon-Agent/pkg/protocol"
)

// Exporter is the pipeline's sink stage: it ships batches over QUIC with
// reconnect and exponential backoff (plus jitter).
//
// Note on semantics: while there is no WAL, a failed batch is handed back to
// the batch processor. Once WAL lands the retry policy becomes "keep retrying,
// the data is durable" instead of "give up after N attempts".
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

func (e *Exporter) Export(ctx context.Context, req *protocol.ExportMetricsRequest) error {
	if req == nil || len(req.ScopeMetrics) == 0 {
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

		if err := e.client.SendBatchMetrics(ctx, req); err != nil {
			lastErr = fmt.Errorf("failed to send batch metric: %w", err)
			continue
		}
		return nil
	}
	return fmt.Errorf("failed to send metric after %d attempts: %w", e.maxRetries, lastErr)
}
