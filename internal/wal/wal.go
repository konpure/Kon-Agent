// Package wal implements an append-only, segmented write-ahead log with
// per-record CRC32, modelled on Prometheus tsdb/wal:
//
//   - records are written to fixed-size rolling segment files (00000000, ...)
//   - replay scans segments in order and validates every record's CRC
//   - a damaged record truncates the log at the last good position
//   - a checkpoint file stores the acknowledged consume position
//
// Data flows in before any send attempt, so backpressure accumulates on disk
// instead of in memory (Prometheus remote write queue model).
package wal

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config holds WAL parameters.
type Config struct {
	Dir           string
	SegmentSize   int64 // bytes per segment file
	MaxSize       int64 // hard cap of total on-disk size
	FsyncInterval time.Duration
}

// Default values applied by Open when the config leaves them unset.
const (
	DefaultSegmentSize   = 16 << 20  // 16 MiB; smaller than Prometheus' 128MB, edge-friendly
	DefaultMaxSize       = 128 << 20 // 128 MiB
	DefaultFsyncInterval = time.Second
)

// WAL is a single-writer, single-reader write-ahead log.
type WAL struct {
	cfg Config

	mu      sync.Mutex
	active  *os.File
	segment uint64
	offset  int64
	dirty   bool
	closed  bool

	stopSync chan struct{}
	syncDone chan struct{}
}

// Open creates or resumes a WAL in cfg.Dir.
func Open(cfg Config) (*WAL, error) {
	if cfg.SegmentSize <= 0 {
		cfg.SegmentSize = DefaultSegmentSize
	}
	if cfg.MaxSize <= 0 {
		cfg.MaxSize = DefaultMaxSize
	}
	if cfg.FsyncInterval <= 0 {
		cfg.FsyncInterval = DefaultFsyncInterval
	}
	if err := os.MkdirAll(cfg.Dir, 0755); err != nil {
		return nil, fmt.Errorf("wal: create dir: %w", err)
	}

	segments, err := listSegments(cfg.Dir)
	if err != nil {
		return nil, err
	}

	w := &WAL{
		cfg:      cfg,
		stopSync: make(chan struct{}),
		syncDone: make(chan struct{}),
	}

	var seg uint64
	if len(segments) > 0 {
		seg = segments[len(segments)-1]
	}
	if err := w.openSegmentLocked(seg); err != nil {
		return nil, err
	}

	go w.fsyncLoop()
	return w, nil
}

// Append writes one record, rotating the segment when the size cap is hit.
func (w *WAL) Append(flags uint8, payload []byte) error {
	rec := encodeRecord(flags, payload)

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return fmt.Errorf("wal: closed")
	}
	if w.offset > 0 && w.offset+int64(len(rec)) > w.cfg.SegmentSize {
		if err := w.syncLocked(); err != nil {
			return err
		}
		if err := w.openSegmentLocked(w.segment + 1); err != nil {
			return err
		}
	}
	if _, err := w.active.Write(rec); err != nil {
		return fmt.Errorf("wal: append: %w", err)
	}
	w.offset += int64(len(rec))
	w.dirty = true
	return nil
}

// Sync forces the active segment to stable storage.
func (w *WAL) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.syncLocked()
}

// Close fsyncs, closes the active segment and stops the fsync loop.
func (w *WAL) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	syncErr := w.syncLocked()
	closeErr := w.active.Close()
	w.mu.Unlock()

	close(w.stopSync)
	<-w.syncDone

	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func (w *WAL) Dir() string        { return w.cfg.Dir }
func (w *WAL) MaxSize() int64     { return w.cfg.MaxSize }
func (w *WAL) SegmentSize() int64 { return w.cfg.SegmentSize }

// Stats returns total bytes on disk and bytes not yet acknowledged by cp.
func (w *WAL) Stats(cp Checkpoint) (total, unacked int64, err error) {
	segments, err := listSegments(w.cfg.Dir)
	if err != nil {
		return 0, 0, err
	}
	for _, s := range segments {
		st, err := os.Stat(segmentPath(w.cfg.Dir, s))
		if err != nil {
			continue
		}
		total += st.Size()
		switch {
		case s > cp.Segment:
			unacked += st.Size()
		case s == cp.Segment && st.Size() > cp.Offset:
			unacked += st.Size() - cp.Offset
		}
	}
	return total, unacked, nil
}

// Truncate cuts the log back to cp after corruption was found there,
// dropping the damaged tail and all later segments.
func (w *WAL) Truncate(cp Checkpoint) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	segments, err := listSegments(w.cfg.Dir)
	if err != nil {
		return err
	}
	for _, s := range segments {
		if s > cp.Segment {
			if err := os.Remove(segmentPath(w.cfg.Dir, s)); err != nil {
				return fmt.Errorf("wal: remove segment %d: %w", s, err)
			}
		}
	}
	if err := os.Truncate(segmentPath(w.cfg.Dir, cp.Segment), cp.Offset); err != nil {
		return fmt.Errorf("wal: truncate: %w", err)
	}
	if w.segment >= cp.Segment {
		if err := w.openSegmentLocked(cp.Segment); err != nil {
			return err
		}
	}
	return nil
}

// EnforceMaxSize deletes oldest segments while total size exceeds MaxSize.
// Fully acked segments go first; if still over the cap, unacked data is
// dropped with a loud warning — collection is never blocked (graceful
// degradation, cf. Monarch). If the checkpoint's segment is deleted, the
// returned checkpoint advances past it.
func (w *WAL) EnforceMaxSize(cp Checkpoint) (Checkpoint, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	segments, err := listSegments(w.cfg.Dir)
	if err != nil {
		return cp, err
	}
	sizes := make(map[uint64]int64, len(segments))
	var total int64
	for _, s := range segments {
		st, err := os.Stat(segmentPath(w.cfg.Dir, s))
		if err != nil {
			continue
		}
		sizes[s] = st.Size()
		total += st.Size()
	}

	for _, s := range segments {
		if total <= w.cfg.MaxSize {
			break
		}
		if s == w.segment {
			slog.Warn("WAL over max size but only the active segment remains", "total", total)
			break
		}
		if s >= cp.Segment {
			slog.Warn("WAL over max size, dropping UNACKED segment (data loss)",
				"segment", s, "bytes", sizes[s])
		}
		if err := os.Remove(segmentPath(w.cfg.Dir, s)); err != nil {
			return cp, fmt.Errorf("wal: enforce max size: %w", err)
		}
		total -= sizes[s]
		if s == cp.Segment {
			cp = Checkpoint{Segment: s + 1, Offset: 0}
		}
	}
	return cp, nil
}

func (w *WAL) syncLocked() error {
	if !w.dirty {
		return nil
	}
	if err := w.active.Sync(); err != nil {
		return fmt.Errorf("wal: fsync: %w", err)
	}
	w.dirty = false
	return nil
}

func (w *WAL) fsyncLoop() {
	defer close(w.syncDone)
	ticker := time.NewTicker(w.cfg.FsyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			w.mu.Lock()
			if !w.closed {
				if err := w.syncLocked(); err != nil {
					slog.Error("WAL periodic fsync failed", "error", err)
				}
			}
			w.mu.Unlock()
		case <-w.stopSync:
			return
		}
	}
}

func (w *WAL) openSegmentLocked(seg uint64) error {
	f, err := os.OpenFile(segmentPath(w.cfg.Dir, seg), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("wal: open segment %d: %w", seg, err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	if w.active != nil {
		_ = w.active.Close()
	}
	w.active = f
	w.segment = seg
	w.offset = st.Size()
	return nil
}

func segmentPath(dir string, seg uint64) string {
	return filepath.Join(dir, fmt.Sprintf("%08d", seg))
}

func listSegments(dir string) ([]uint64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("wal: list segments: %w", err)
	}
	var segments []uint64
	for _, e := range entries {
		if e.IsDir() || len(e.Name()) != 8 {
			continue
		}
		n, err := strconv.ParseUint(e.Name(), 10, 64)
		if err != nil {
			continue
		}
		segments = append(segments, n)
	}
	sort.Slice(segments, func(i, j int) bool { return segments[i] < segments[j] })
	return segments, nil
}

// ParseSize parses strings like "16MB" or "1GB" into bytes. Binary units
// (MiB) are used for the MB/GB suffixes, matching operational convention.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	multiplier := int64(1)
	switch {
	case strings.HasSuffix(s, "GIB"):
		multiplier, s = 1<<30, strings.TrimSuffix(s, "GIB")
	case strings.HasSuffix(s, "GB"):
		multiplier, s = 1<<30, strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "MIB"):
		multiplier, s = 1<<20, strings.TrimSuffix(s, "MIB")
	case strings.HasSuffix(s, "MB"):
		multiplier, s = 1<<20, strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "KIB"):
		multiplier, s = 1<<10, strings.TrimSuffix(s, "KIB")
	case strings.HasSuffix(s, "KB"):
		multiplier, s = 1<<10, strings.TrimSuffix(s, "KB")
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	}
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("wal: invalid size %q", s)
	}
	return v * multiplier, nil
}
