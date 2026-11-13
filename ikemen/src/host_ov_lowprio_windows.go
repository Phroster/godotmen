package main

import (
	"runtime"
	"syscall"
)

var ovProcSetThreadPriority = syscall.NewLazyDLL("kernel32.dll").NewProc("SetThreadPriority")

// ovLowPriorityThread: the calling goroutine keeps its own OS thread from here on, run at THREAD_PRIORITY_LOWEST - a
// background builder (host_ov_relief.go's relief workers) takes only the CPU the game leaves, never a frame's time
func ovLowPriorityThread() {
	runtime.LockOSThread()
	const currentThread = ^uintptr(1)                             // (GetCurrentThread's pseudo-handle: -2)
	ovProcSetThreadPriority.Call(currentThread, ^uintptr(1)) // (THREAD_PRIORITY_LOWEST: -2)
}
