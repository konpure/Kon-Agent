package codec

import (
	"fmt"
	"math"
	"math/bits"
)

// seriesState is the per-series stream state mirrored by encoder and decoder.
// It lives for the lifetime of one QUIC connection; both sides start empty on
// (re)connect, which is what makes WAL replay safe: records are stored raw and
// re-encoded against the state of whatever connection replays them.
type seriesState struct {
	initialized bool
	lastTS      int64
	lastDelta   int64
	lastVal     uint64
	lz, tz      uint8
	hasWindow   bool
	histBounds  []float64
}

// ---------------------------------------------------------------------------
// Delta-of-delta timestamp coding (Gorilla, VLDB 2015, section 4.1):
//   dod == 0          -> '0'                          (1 bit)
//   [-63, 64]         -> '10'  + 7 bits  (biased +63)
//   [-255, 256]       -> '110' + 9 bits  (biased +255)
//   [-2047, 2048]     -> '1110'+ 12 bits (biased +2047)
//   fits int32        -> '1111'+'0' + 32 bits (two's complement)
//   otherwise         -> '1111'+'1' + 64 bits (two's complement)
// Fixed-interval collection makes dod ~0, so a point costs about 1 bit.
//
// The 64-bit tier is a correctness requirement, not a formality: timestamps
// are UnixNano, so the first delta after a keyframe equals the collection
// period (a 5s period is 5e9 ns, 2.3x past int32). Without the wide tier the
// value was silently truncated to its low 32 bits, and because the decoder
// folds dod into a running lastDelta, every later timestamp on that
// connection inherited the error until the next keyframe.
// ---------------------------------------------------------------------------

func writeDod(w *bitWriter, dod int64) {
	switch {
	case dod == 0:
		w.writeBit(false)
	case dod >= -63 && dod <= 64:
		w.writeBits(0b10, 2)
		w.writeBits(uint64(dod+63), 7)
	case dod >= -255 && dod <= 256:
		w.writeBits(0b110, 3)
		w.writeBits(uint64(dod+255), 9)
	case dod >= -2047 && dod <= 2048:
		w.writeBits(0b1110, 4)
		w.writeBits(uint64(dod+2047), 12)
	default:
		// '1111' opens the wide tier; one more bit selects its width.
		w.writeBits(0b1111, 4)
		if int64(int32(dod)) == dod {
			w.writeBit(false)
			w.writeBits(uint64(int32(dod)), 32)
			return
		}
		w.writeBit(true)
		w.writeBits(uint64(dod), 64)
	}
}

func readDod(r *bitReader) (int64, error) {
	b0, err := r.readBit()
	if err != nil {
		return 0, err
	}
	if !b0 {
		return 0, nil
	}
	b1, err := r.readBit()
	if err != nil {
		return 0, err
	}
	if !b1 { // '10'
		v, err := r.readBits(7)
		return int64(v) - 63, err
	}
	b2, err := r.readBit()
	if err != nil {
		return 0, err
	}
	if !b2 { // '110'
		v, err := r.readBits(9)
		return int64(v) - 255, err
	}
	b3, err := r.readBit()
	if err != nil {
		return 0, err
	}
	if !b3 { // '1110'
		v, err := r.readBits(12)
		return int64(v) - 2047, err
	}
	// '1111': wide tier, one more bit selects 32 or 64 bits.
	wide, err := r.readBit()
	if err != nil {
		return 0, err
	}
	if !wide {
		v, err := r.readBits(32)
		return int64(int32(uint32(v))), err
	}
	v, err := r.readBits(64)
	return int64(v), err
}

// ---------------------------------------------------------------------------
// XOR value coding (Gorilla, section 4.2): consecutive float64 samples share
// most of their bits, so only the significant middle of the XOR is stored.
//   xor == 0            -> '0'                    (1 bit, value unchanged)
//   fits previous window-> '10' + significant bits (window reuse)
//   otherwise           -> '11' + 5-bit leading zeros + 6-bit significant
//                          length + significant bits
// ---------------------------------------------------------------------------

func writeValueXOR(w *bitWriter, st *seriesState, val float64) {
	cur := math.Float64bits(val)
	xor := cur ^ st.lastVal
	st.lastVal = cur

	if xor == 0 {
		w.writeBit(false)
		return
	}

	lz := uint8(bits.LeadingZeros64(xor))
	tz := uint8(bits.TrailingZeros64(xor))

	w.writeBit(true)
	if st.hasWindow && lz >= st.lz && tz >= st.tz {
		w.writeBit(false) // reuse previous window
		sig := 64 - st.lz - st.tz
		w.writeBits(xor>>st.tz, uint(sig))
		return
	}

	w.writeBit(true) // new window
	if lz >= 32 {
		lz = 31 // 5-bit field cap; the extra leading bit folds into the payload
	}
	w.writeBits(uint64(lz), 5)
	sig := 64 - lz - tz
	w.writeBits(uint64(sig%64), 6) // 64 is stored as 0
	w.writeBits(xor>>tz, uint(sig))
	st.lz, st.tz, st.hasWindow = lz, tz, true
}

func readValueXOR(r *bitReader, st *seriesState) (float64, error) {
	changed, err := r.readBit()
	if err != nil {
		return 0, err
	}
	if !changed {
		return math.Float64frombits(st.lastVal), nil
	}

	newWindow, err := r.readBit()
	if err != nil {
		return 0, err
	}

	var lz, tz uint8
	var sig uint
	if !newWindow {
		if !st.hasWindow {
			return 0, fmt.Errorf("codec: xor window referenced before keyframe")
		}
		lz, tz = st.lz, st.tz
		sig = uint(64 - lz - tz)
	} else {
		v, err := r.readBits(5)
		if err != nil {
			return 0, err
		}
		lz = uint8(v)
		s, err := r.readBits(6)
		if err != nil {
			return 0, err
		}
		sig = uint(s)
		if sig == 0 {
			sig = 64
		}
		tzInt := 64 - int(lz) - int(sig)
		if tzInt < 0 {
			return 0, fmt.Errorf("codec: corrupt xor header (lz=%d sig=%d)", lz, sig)
		}
		tz = uint8(tzInt)
		st.lz, st.tz, st.hasWindow = lz, tz, true
	}

	xorbits, err := r.readBits(sig)
	if err != nil {
		return 0, err
	}
	st.lastVal ^= xorbits << tz
	return math.Float64frombits(st.lastVal), nil
}
