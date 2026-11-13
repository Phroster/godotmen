//go:build overlay

package main

/*
#include <stdint.h>
*/
import "C"

import "unsafe"

//export ikemen_mix_audio
func ikemen_mix_audio(interleavedStereo *C.float, frames C.int, mixRate C.int) (result C.int) {
	var out []float32
	defer func() {
		if recover() != nil {
			silenceHostAudio(out) // Discard even a partial block after a mixer panic.
			hostAudioError.Store(hostAudioErrPanic)
			result = C.int(hostAudioErrPanic)
		}
	}()
	if frames < 0 || frames > hostAudioMaxFrames || (frames > 0 && interleavedStereo == nil) {
		hostAudioError.Store(hostAudioErrArgument)
		return C.int(hostAudioErrArgument)
	}
	if frames > 0 {
		// C owns this memory. We retain neither its address nor any Go pointer
		// in it, and the mix/clear finishes before the exported call returns.
		out = unsafe.Slice((*float32)(unsafe.Pointer(interleavedStereo)), int(frames)*2)
	}
	return C.int(mixHostAudio(out, int(frames), int(mixRate)))
}

// Also exercises the actual recover-wrapped C ABI body in the package tests.
func hostAudioExportForTest(dst []float32, frames, rate int) int {
	var ptr *C.float
	if len(dst) > 0 {
		ptr = (*C.float)(unsafe.Pointer(&dst[0]))
	}
	return int(ikemen_mix_audio(ptr, C.int(frames), C.int(rate)))
}
