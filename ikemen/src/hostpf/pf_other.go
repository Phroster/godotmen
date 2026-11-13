//go:build !amd64

package hostpf

import "unsafe"

// Lines: no prefetch on this platform
func Lines(p unsafe.Pointer, offs *uint16, n int) {}

// T0x4: no prefetch on this platform
func T0x4(a, b, c, d unsafe.Pointer) {}

// On: the no-op fallback
const On = false
