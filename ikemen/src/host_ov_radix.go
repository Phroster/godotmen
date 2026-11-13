package main

import "slices"

// ovRadixSort64: keys sorted ascending in place - LSD radix, 8 bits a pass, a pass skipped where every key has the same
// byte; scratch (at least len(keys) long, else grown) is returned for reuse. The draw capture's
// depth order (ovDepthSort) sorted ~3,000 keys a tick late in a large fight with slices.Sort's comparisons and their branch
// misses. The result is THE ascending order of the keys, the same as slices.Sort's: a sorted sequence of given values is
// unique (equal keys, which ovDepthSort's position field rules out anyway, are equal values either way).
func ovRadixSort64(keys, scratch []uint64) []uint64 {
	n := len(keys)
	if n < 256 {
		slices.Sort(keys)
		return scratch
	}
	if cap(scratch) < n {
		scratch = make([]uint64, n)
	}
	scratch = scratch[:n]
	var count [8][256]int32
	for _, k := range keys {
		count[0][byte(k)]++
		count[1][byte(k>>8)]++
		count[2][byte(k>>16)]++
		count[3][byte(k>>24)]++
		count[4][byte(k>>32)]++
		count[5][byte(k>>40)]++
		count[6][byte(k>>48)]++
		count[7][byte(k>>56)]++
	}
	src, dst := keys, scratch
	for b := uint(0); b < 8; b++ {
		c := &count[b]
		shift := 8 * b
		if c[byte(src[0]>>shift)] == int32(n) {
			continue // (every key has this byte: the pass would leave the order as it is)
		}
		var sum int32
		for i := range c {
			t := c[i]
			c[i] = sum
			sum += t
		}
		for _, k := range src {
			d := byte(k >> shift)
			dst[c[d]] = k
			c[d]++
		}
		src, dst = dst, src
	}
	if &src[0] != &keys[0] {
		copy(keys, src)
	}
	return scratch
}
