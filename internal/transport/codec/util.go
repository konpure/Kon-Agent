package codec

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
)

// zigzag maps signed ints to unsigned for varint coding (proto-style).
func zigzag(v int64) uint64 {
	return uint64(v<<1) ^ uint64(v>>63)
}

func unzigzag(u uint64) int64 {
	return int64(u>>1) ^ -int64(u&1)
}

func writeUvarint(buf *bytes.Buffer, v uint64) {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], v)
	buf.Write(tmp[:n])
}

func writeFloat64(buf *bytes.Buffer, f float64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], math.Float64bits(f))
	buf.Write(b[:])
}

func readFloat64(r io.Reader) (float64, error) {
	var b [8]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return math.Float64frombits(binary.BigEndian.Uint64(b[:])), nil
}
