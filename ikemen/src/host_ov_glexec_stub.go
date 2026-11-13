//go:build !overlay

package main

// Overlay: builds without the overlay DLL (a stand-alone exe) have no GL command queue
// executor; the fast sprite path then calls GL directly (host_ov_render.go).

func ovGLExecInit() bool { return false }

func ovGLExec(ops *uint32, n int) int { return 0 }
