//go:build !overlay

package main

import "time"

// What a stand-alone build (a plain Ikemen GO executable, without the build tag overlay) needs from the host files.
// The real versions are in host_ov_api.go (build tag overlay).

const ovBuild = false

func ovCurrentThreadID() uint64 { return 0 }

const ovTidFast = false

func ovTimingUs() int64 { return time.Now().UnixNano() / 1000 }

func ovApplyAudioPause() {}
