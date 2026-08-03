package rank

// sortDescByScore returns entity ids ordered by score, highest first.
//
// An LSD radix sort rather than sort.Slice: at 2.7M members a comparison sort spends
// most of its time on indirect loads and branch mispredictions, while radix is linear
// and allocation-free once the buffers exist. Passes whose digit is uniform across
// the input are skipped, which matters here because scores top out around 8×10^12 —
// the upper bytes are identical for every donor, so most passes are elided.
//
// # Why the key is inverted rather than the result reversed
//
// Radix is stable and produces *ascending* order, so seeding with ids in ascending
// order leaves tie groups in ascending id order. Reversing the finished array would
// fix the score direction but flip ties into descending id order — and with millions
// of donors tied on zero, that ordering has to be stable or rank movement becomes
// pure noise between cycles. Sorting on ^score instead gives descending scores while
// preserving ascending ids within a tie, because inverting the bits of a
// non-negative integer reverses its order.
//
// The returned slice aliases buf and is only valid until the next call.
func sortDescByScore(scores []int64, buf *sortBuf) []int32 {
	n := len(scores)
	if n == 0 {
		return nil
	}
	buf.reset(n)
	src, dst := buf.a, buf.b
	for i := 0; i < n; i++ {
		src[i] = int32(i)
	}

	// Scores are cumulative lifetime totals and never negative, so the inverted
	// bits order the same as the negated values.
	key := func(id int32) uint64 { return ^uint64(scores[id]) }

	var counts [256]int
	for shift := uint(0); shift < 64; shift += 8 {
		for i := range counts {
			counts[i] = 0
		}
		for i := 0; i < n; i++ {
			counts[byte(key(int32(i))>>shift)]++
		}
		if counts[byte(key(src[0])>>shift)] == n {
			continue // every entity shares this digit; the pass is a no-op
		}

		sum := 0
		for i := 0; i < 256; i++ {
			c := counts[i]
			counts[i] = sum
			sum += c
		}
		for i := 0; i < n; i++ {
			id := src[i]
			d := byte(key(id) >> shift)
			dst[counts[d]] = id
			counts[d]++
		}
		src, dst = dst, src
	}
	return src[:n]
}

// sortBuf holds the two ping-pong buffers so a cycle's sort allocates nothing.
type sortBuf struct {
	a, b []int32
}

func (s *sortBuf) reset(n int) {
	if cap(s.a) < n {
		s.a = make([]int32, n)
		s.b = make([]int32, n)
	}
	s.a = s.a[:n]
	s.b = s.b[:n]
}
