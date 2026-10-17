package codec

import "io"

// bitWriter packs bits MSB-first (Gorilla block convention).
type bitWriter struct {
	buf   []byte
	cur   byte
	nbits uint8
}

func (w *bitWriter) writeBit(b bool) {
	w.cur <<= 1
	if b {
		w.cur |= 1
	}
	w.nbits++
	if w.nbits == 8 {
		w.buf = append(w.buf, w.cur)
		w.cur, w.nbits = 0, 0
	}
}

// writeBits writes the low n bits of v, MSB first.
func (w *bitWriter) writeBits(v uint64, n uint) {
	for i := n; i > 0; i-- {
		w.writeBit(v>>(i-1)&1 == 1)
	}
}

// bytes flushes the partial trailing byte (zero-padded).
func (w *bitWriter) bytes() []byte {
	if w.nbits > 0 {
		w.buf = append(w.buf, w.cur<<(8-w.nbits))
		w.cur, w.nbits = 0, 0
	}
	return w.buf
}

type bitReader struct {
	data []byte
	pos  int
	off  uint8
}

func (r *bitReader) readBit() (bool, error) {
	if r.pos >= len(r.data) {
		return false, io.ErrUnexpectedEOF
	}
	b := r.data[r.pos]>>(7-r.off)&1 == 1
	r.off++
	if r.off == 8 {
		r.off, r.pos = 0, r.pos+1
	}
	return b, nil
}

func (r *bitReader) readBits(n uint) (uint64, error) {
	var v uint64
	for i := uint(0); i < n; i++ {
		b, err := r.readBit()
		if err != nil {
			return 0, err
		}
		v <<= 1
		if b {
			v |= 1
		}
	}
	return v, nil
}
