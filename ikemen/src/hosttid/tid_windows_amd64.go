// Package hosttid reads the calling OS thread's ID without a cgo call (host overlay: ovCapturing asks it for
// every draw op of a snapshot capture; through cgo GetCurrentThreadId cost ~0.35 ms of a 101-fighter fight's tick).
// A separate package because a package using cgo cannot hold Go assembly.
package hosttid

// ID returns the calling OS thread's ID, GetCurrentThreadId's value: TEB.ClientId.UniqueThread, read through GS.
// As with GetCurrentThreadId, a goroutine not locked to its thread can move to another thread right after the call.
func ID() uint64
