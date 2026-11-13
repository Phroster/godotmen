package main

import (
	"bufio"
	"encoding/binary"
	"math"
	"os"
	"sync"
)

// Dev diagnostic, env IKEMEN_AUDIO_DUMP=<file>: Ikemen's own mix exactly as the host plays it (mixHostAudio: float32
// stereo at the mix rate, block after block from the audio thread, silent blocks included) written raw to <file>.
// Unset, nothing is written.
var (
	ovAudioDumpOn = os.Getenv("IKEMEN_AUDIO_DUMP") != ""
	ovAudioDumpMu sync.Mutex
	ovAudioDumpW  *bufio.Writer
)

func ovAudioDumpWrite(dst []float32, frames, rate int) {
	if !ovAudioDumpOn || frames <= 0 || len(dst) < frames*2 {
		return
	}
	ovAudioDumpMu.Lock()
	defer ovAudioDumpMu.Unlock()
	if ovAudioDumpW == nil {
		f, err := os.Create(os.Getenv("IKEMEN_AUDIO_DUMP"))
		if err != nil {
			return
		}
		ovAudioDumpW = bufio.NewWriterSize(f, 1<<20)
		ovLog("audio dump: %s, float32 stereo at %d Hz", os.Getenv("IKEMEN_AUDIO_DUMP"), rate)
	}
	var b [4]byte
	for _, v := range dst[:frames*2] {
		binary.LittleEndian.PutUint32(b[:], math.Float32bits(v))
		ovAudioDumpW.Write(b[:])
	}
	ovAudioDumpW.Flush() // (dev only: the run may end at any block)
}
