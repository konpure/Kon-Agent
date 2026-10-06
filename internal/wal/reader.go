package wal

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
)

// Reader replays records in segment order starting from a checkpoint.
// It is not safe for concurrent use; the single consumer goroutine owns it.
type Reader struct {
	dir string
	seg uint64
	off int64
	f   *os.File
}

// NewReader creates a reader positioned right after the given checkpoint.
func NewReader(dir string, from Checkpoint) *Reader {
	return &Reader{dir: dir, seg: from.Segment, off: from.Offset}
}

// Reset repositions the reader (after truncation or retention).
func (r *Reader) Reset(cp Checkpoint) {
	r.closeFile()
	r.seg = cp.Segment
	r.off = cp.Offset
}

// Next returns the next record and the position right after it.
// io.EOF means the reader has caught up with the writer; a *CorruptionError
// means the log is damaged at the reader's current position.
func (r *Reader) Next() (flags uint8, payload []byte, next Checkpoint, err error) {
	for {
		if err := r.ensureOpen(); err != nil {
			if err == io.EOF {
				return 0, nil, Checkpoint{}, io.EOF
			}
			return 0, nil, Checkpoint{}, err
		}

		header := make([]byte, recordHeaderSize)
		n, herr := io.ReadFull(r.f, header)
		if herr == io.EOF || herr == io.ErrUnexpectedEOF {
			if n == 0 {
				// Clean end of segment: hop to the next one if it exists.
				if r.advanceSegment() {
					continue
				}
				return 0, nil, Checkpoint{}, io.EOF
			}
			return 0, nil, Checkpoint{}, &CorruptionError{Segment: r.seg, Offset: r.off, Reason: "torn record header"}
		}
		if herr != nil {
			return 0, nil, Checkpoint{}, herr
		}

		flags = header[0]
		length := binary.BigEndian.Uint32(header[1:5])
		crc := binary.BigEndian.Uint32(header[5:9])
		if length > maxRecordSize {
			return 0, nil, Checkpoint{}, &CorruptionError{Segment: r.seg, Offset: r.off, Reason: fmt.Sprintf("absurd length %d", length)}
		}
		payload = make([]byte, length)
		if _, err := io.ReadFull(r.f, payload); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return 0, nil, Checkpoint{}, &CorruptionError{Segment: r.seg, Offset: r.off, Reason: "torn record payload"}
			}
			return 0, nil, Checkpoint{}, err
		}
		if crc32.ChecksumIEEE(payload) != crc {
			return 0, nil, Checkpoint{}, &CorruptionError{Segment: r.seg, Offset: r.off, Reason: "crc32 mismatch"}
		}

		next = Checkpoint{Segment: r.seg, Offset: r.off + recordHeaderSize + int64(length)}
		r.off = next.Offset
		return flags, payload, next, nil
	}
}

// Close releases the underlying file handle.
func (r *Reader) Close() {
	r.closeFile()
}

func (r *Reader) ensureOpen() error {
	for r.f == nil {
		f, err := os.Open(segmentPath(r.dir, r.seg))
		if os.IsNotExist(err) {
			if !r.advanceSegment() {
				return io.EOF
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("wal: reader open segment %d: %w", r.seg, err)
		}
		if _, err := f.Seek(r.off, io.SeekStart); err != nil {
			_ = f.Close()
			return err
		}
		r.f = f
	}
	return nil
}

// advanceSegment switches to the next segment if it exists.
func (r *Reader) advanceSegment() bool {
	if _, err := os.Stat(segmentPath(r.dir, r.seg+1)); err != nil {
		return false
	}
	r.closeFile()
	r.seg++
	r.off = 0
	return true
}

func (r *Reader) closeFile() {
	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
	}
}
