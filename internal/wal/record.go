package wal

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// recordHeaderSize = flags(1) + length(4) + crc32(4).
//
// The flags byte is reserved for the transport codec stage (compression
// marker), so the WAL format does not change when snappy/Gorilla encoding
// lands later.
const recordHeaderSize = 9

// maxRecordSize guards against absurd lengths after corruption.
const maxRecordSize = 32 << 20

// encodeRecord builds [flags][length][crc32(payload)][payload].
func encodeRecord(flags uint8, payload []byte) []byte {
	buf := make([]byte, recordHeaderSize+len(payload))
	buf[0] = flags
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	binary.BigEndian.PutUint32(buf[5:9], crc32.ChecksumIEEE(payload))
	copy(buf[recordHeaderSize:], payload)
	return buf
}

// CorruptionError marks a damaged record at a WAL position. Callers are
// expected to truncate the log at the last good position (tsdb/wal behavior:
// repair by truncation, never drop the whole log).
type CorruptionError struct {
	Segment uint64
	Offset  int64
	Reason  string
}

func (e *CorruptionError) Error() string {
	return fmt.Sprintf("wal: corrupt record at segment %d offset %d: %s", e.Segment, e.Offset, e.Reason)
}
