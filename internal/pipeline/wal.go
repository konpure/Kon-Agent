package pipeline

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/konpure/Kon-Agent/internal/wal"
)

// walConsumer replays WAL records in order and exports them, advancing the
// ack checkpoint only after a successful send. This is the Prometheus remote
// write model: the WAL is the queue, backpressure accumulates on disk, and a
// successful send is what moves the consume position forward.
type walConsumer struct {
	w        *wal.WAL
	exporter *Exporter
	reader   *wal.Reader
	cp       wal.Checkpoint

	notifyCh chan struct{}
	stopCh   chan struct{}
	doneCh   chan struct{}

	onError    func(errorType string)
	onPressure func(degraded bool)

	degraded bool
	backoff  time.Duration
}

func newWALConsumer(w *wal.WAL, exporter *Exporter, onError func(string), onPressure func(bool)) (*walConsumer, error) {
	cp, err := wal.LoadCheckpoint(w.Dir())
	if err != nil {
		return nil, err
	}
	return &walConsumer{
		w:          w,
		exporter:   exporter,
		reader:     wal.NewReader(w.Dir(), cp),
		cp:         cp,
		notifyCh:   make(chan struct{}, 1),
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
		onError:    onError,
		onPressure: onPressure,
	}, nil
}

// notify wakes the consumer; non-blocking since a pending wake-up is enough.
func (c *walConsumer) notify() {
	select {
	case c.notifyCh <- struct{}{}:
	default:
	}
}

func (c *walConsumer) run(ctx context.Context) {
	defer close(c.doneCh)

	idle := time.NewTicker(time.Second)
	defer idle.Stop()

	for {
		c.drain(ctx)
		select {
		case <-c.notifyCh:
		case <-idle.C:
		case <-c.stopCh:
			slog.Info("WAL consumer draining before exit")
			c.drain(ctx)
			return
		case <-ctx.Done():
			return
		}
	}
}

// stop asks the consumer to drain and exit, then waits for it.
func (c *walConsumer) stop() {
	close(c.stopCh)
	<-c.doneCh
	c.reader.Close()
}

func (c *walConsumer) drain(ctx context.Context) {
	for {
		_, payload, next, err := c.reader.Next()
		if errors.Is(err, io.EOF) {
			c.reportPressure()
			return
		}
		var corrupt *wal.CorruptionError
		if errors.As(err, &corrupt) {
			slog.Error("WAL record corrupt, truncating at last good position", "error", err)
			c.onError("wal_corruption")
			if terr := c.w.Truncate(c.cp); terr != nil {
				slog.Error("WAL truncate failed", "error", terr)
				c.onError("wal_truncate_failed")
				return
			}
			c.reader.Reset(c.cp)
			continue
		}
		if err != nil {
			slog.Error("WAL read failed", "error", err)
			c.onError("wal_read_failed")
			return
		}

		if err := c.exporter.ExportBytes(ctx, payload); err != nil {
			slog.Warn("WAL replay export failed, backing off", "error", err, "backoff", c.backoff)
			c.onError("export_failed")
			if !c.sleep(ctx) {
				return
			}
			continue // retry the same record; the checkpoint does not move
		}

		c.cp = next
		if err := wal.SaveCheckpoint(c.w.Dir(), c.cp); err != nil {
			slog.Error("WAL checkpoint save failed", "error", err)
			c.onError("wal_checkpoint_failed")
		}
		c.backoff = 0
		c.reportPressure()
	}
}

// sleep backs off between export retries; data is durable so retry is unbounded.
func (c *walConsumer) sleep(ctx context.Context) bool {
	if c.backoff == 0 {
		c.backoff = 200 * time.Millisecond
	} else {
		c.backoff *= 2
	}
	if c.backoff > 30*time.Second {
		c.backoff = 30 * time.Second
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(c.backoff):
		return true
	}
}

// reportPressure toggles the degradation flag with hysteresis (enter when
// unacked data exceeds 70% of MaxSize, exit below 50%) and enforces the hard
// size cap. The sampling processor will consult the flag to drop granularity.
func (c *walConsumer) reportPressure() {
	total, unacked, err := c.w.Stats(c.cp)
	if err != nil {
		return
	}
	maxSize := c.w.MaxSize()

	if total > maxSize {
		slog.Warn("WAL size over hard cap, enforcing retention", "total", total, "max", maxSize)
		newCp, err := c.w.EnforceMaxSize(c.cp)
		if err != nil {
			slog.Error("WAL retention failed", "error", err)
			c.onError("wal_enforce_failed")
		} else if newCp != c.cp {
			c.cp = newCp
			c.reader.Reset(newCp)
			if err := wal.SaveCheckpoint(c.w.Dir(), newCp); err != nil {
				slog.Error("WAL checkpoint save failed", "error", err)
			}
		}
	}

	degraded := c.degraded
	switch {
	case !degraded && unacked > int64(float64(maxSize)*0.7):
		degraded = true
	case degraded && unacked < int64(float64(maxSize)*0.5):
		degraded = false
	}
	if degraded != c.degraded {
		c.degraded = degraded
		slog.Warn("WAL backpressure state changed", "degraded", degraded, "unacked_bytes", unacked)
		if c.onPressure != nil {
			c.onPressure(degraded)
		}
	}
}
