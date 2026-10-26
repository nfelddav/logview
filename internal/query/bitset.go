package query

import "math/bits"

// Bitset is a growable set of line indexes (one bit per line, so a filter
// over 500M lines costs ~60MB).
type Bitset struct {
	w []uint64
	n int64
}

func (b *Bitset) Set(i int64) {
	k := int(i >> 6)
	for k >= len(b.w) {
		b.w = append(b.w, make([]uint64, len(b.w)+1)...)
	}
	m := uint64(1) << (i & 63)
	if b.w[k]&m == 0 {
		b.w[k] |= m
		b.n++
	}
}

func (b *Bitset) Clear(i int64) {
	k := int(i >> 6)
	if k >= len(b.w) {
		return
	}
	m := uint64(1) << (i & 63)
	if b.w[k]&m != 0 {
		b.w[k] &^= m
		b.n--
	}
}

func (b *Bitset) Test(i int64) bool {
	k := int(i >> 6)
	return i >= 0 && k < len(b.w) && b.w[k]&(1<<(i&63)) != 0
}

func (b *Bitset) Count() int64 { return b.n }

// NextSet returns the first set index in [from, limit), or -1.
func (b *Bitset) NextSet(from, limit int64) int64 {
	if from < 0 {
		from = 0
	}
	for k := int(from >> 6); k < len(b.w) && int64(k)<<6 < limit; k++ {
		w := b.w[k]
		if k == int(from>>6) {
			w &= ^uint64(0) << (from & 63)
		}
		if w != 0 {
			i := int64(k)<<6 + int64(bits.TrailingZeros64(w))
			if i < limit {
				return i
			}
			return -1
		}
	}
	return -1
}

// PrevSet returns the last set index <= from, or -1.
func (b *Bitset) PrevSet(from int64) int64 {
	if from < 0 {
		return -1
	}
	k := int(from >> 6)
	if k >= len(b.w) {
		k = len(b.w) - 1
		from = int64(k)<<6 + 63
	}
	for ; k >= 0; k-- {
		w := b.w[k]
		if k == int(from>>6) {
			w &= ^uint64(0) >> (63 - from&63)
		}
		if w != 0 {
			return int64(k)<<6 + 63 - int64(bits.LeadingZeros64(w))
		}
	}
	return -1
}

// Rank counts set indexes < i.
func (b *Bitset) Rank(i int64) int64 {
	var n int64
	k := int(i >> 6)
	for j := 0; j < k && j < len(b.w); j++ {
		n += int64(bits.OnesCount64(b.w[j]))
	}
	if k < len(b.w) && i&63 != 0 {
		n += int64(bits.OnesCount64(b.w[k] & (uint64(1)<<(i&63) - 1)))
	}
	return n
}
