// Package hostpf: software prefetch hints (PREFETCHT0) for the host overlay's walks over hundreds of scattered
// heap objects (explods, draw sprites) on the Ikemen thread: the walk asks for the lines a few objects ahead so their
// cache misses overlap instead of queueing one by one. A prefetch is only a hint to the cache: it never faults (nil and
// stale addresses included), writes nothing and changes no result. A separate package because a package using cgo
// cannot hold Go assembly (as hosttid).
package hostpf

import "unsafe"

// Lines asks for the cache lines at p+offs[i], i < n (p nil: nothing).
//
//go:noescape
func Lines(p unsafe.Pointer, offs *uint16, n int)

// T0x4 asks for the lines at four addresses (any may be nil).
//
//go:noescape
func T0x4(a, b, c, d unsafe.Pointer)

// On tells whether these are real prefetches (false: the no-op fallback of other platforms)
const On = true
