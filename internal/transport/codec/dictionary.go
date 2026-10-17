package codec

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

// dictionary is the per-frame string table. Following VictoriaMetrics'
// dictionary-encoding idea, repeated label strings are stored once per frame
// and data points carry integer indexes — label strings dominate the byte
// count of label-heavy batches, so this is where most of the savings are.
type dictionary struct {
	index map[string]uint32
	strs  []string
}

func newDictionary() *dictionary {
	return &dictionary{index: make(map[string]uint32)}
}

// ref returns the index of s, interning it on first sight.
func (d *dictionary) ref(s string) uint32 {
	if i, ok := d.index[s]; ok {
		return i
	}
	i := uint32(len(d.strs))
	d.index[s] = i
	d.strs = append(d.strs, s)
	return i
}

func (d *dictionary) writeTo(buf *bytes.Buffer) {
	writeUvarint(buf, uint64(len(d.strs)))
	for _, s := range d.strs {
		writeUvarint(buf, uint64(len(s)))
		buf.WriteString(s)
	}
}

func readDictionary(r *bytes.Reader) ([]string, error) {
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, err
	}
	if n > 1<<16 {
		return nil, fmt.Errorf("codec: absurd dictionary size %d", n)
	}
	strs := make([]string, 0, n)
	for i := uint64(0); i < n; i++ {
		l, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, err
		}
		if l > 1<<20 {
			return nil, fmt.Errorf("codec: absurd dictionary string length %d", l)
		}
		b := make([]byte, l)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, err
		}
		strs = append(strs, string(b))
	}
	return strs, nil
}
