//go:build !(windows && amd64)

package hosttid

// ID: no fast path on this platform (0: callers keep their own GetCurrentThreadId)
func ID() uint64 { return 0 }
