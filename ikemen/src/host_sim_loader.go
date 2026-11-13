package main

import (
	"sync"
	"sync/atomic"
)

// Asynchronous FFA spawn loads in flight (host_ov_core.go ovStartLoad). While
// any runs, findActiveSff borrows nothing: those goroutines write sys.cgi.
// The mutex also keeps a load from starting during a pre-match scan.
var simAsyncLoads struct {
	sync.Mutex
	n int
}

func simAsyncLoadBegin() {
	simAsyncLoads.Lock()
	simAsyncLoads.n++
	simAsyncLoads.Unlock()
}

func simAsyncLoadEnd() {
	simAsyncLoads.Lock()
	simAsyncLoads.n--
	simAsyncLoads.Unlock()
}

// Loader progress is published by a goroutine and polled by Lua/the host.
// LoaderState has underlying type int32, so the pointer conversion preserves
// the existing field layout without unsafe or a second mirrored state value.
func (l *Loader) simState() LoaderState {
	return LoaderState(atomic.LoadInt32((*int32)(&l.state)))
}

func (l *Loader) simSetState(state LoaderState) {
	atomic.StoreInt32((*int32)(&l.state), int32(state))
}

// Background FFA asset/compiler warnings use the existing engine task queue.
// Console storage and drawing stay on the engine thread.
func simLoadConsole(message string) {
	if ovFFA {
		sys.mainThreadTask <- func() { sys.appendToConsole(message) }
		return
	}
	sys.appendToConsole(message)
}
