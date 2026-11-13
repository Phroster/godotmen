//go:build !windows

package main

import "runtime"

// ovLowPriorityThread: (elsewhere) the goroutine keeps its own OS thread; its priority is left as it is
func ovLowPriorityThread() { runtime.LockOSThread() }
