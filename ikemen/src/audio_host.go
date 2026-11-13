//go:build overlay

package main

import (
	"fmt"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gopxl/beep/v2"
	"github.com/ikemen-engine/Ikemen-GO/src/hostmix"
)

const (
	hostAudioQuantum     = 512 // beep.Mixer's own working block; no additional queue.
	// hostAudioLockBlock: frames mixed per hold of the speaker lock - the Ikemen thread's sound changes wait at most
	// one block's mix, not a whole callback's (10-20 ms waits in a large fight's slow ticks)
	hostAudioLockBlock = 128
	// hostAudioLockWait: how long one callback waits for a busy speaker lock before the rest of its block is silence
	// (it gave silence at once: 17-30% of the callbacks in a 101-fighter fight's busy phases)
	hostAudioLockWait = 2 * time.Millisecond
	// hostAudioYield: how long, in all, one callback gives way between its blocks to the Ikemen side's waiting sound
	// changes (Go's mutex lets the mixer's next TryLock win until a waiter has waited ~1 ms)
	hostAudioYield = 2 * time.Millisecond
	hostAudioMaxFrames   = 16384
	hostAudioErrArgument = -1
	hostAudioErrRate     = -2
	hostAudioErrPanic    = -3
)

// Written before System.init, on the boot thread. Never changed while audio runs.
var ovAudioConfig struct {
	host bool
	rate beep.SampleRate
}

// The callback never reads the non-atomic global speaker or the overlay world's
// mutex. Publication/withdrawal is independent of the simulation rendezvous.
var hostAudioCurrent atomic.Pointer[HostSpeaker]
var hostAudioError atomic.Int32

// ovConfigureAudio must run AFTER loadConfig/normalize and BEFORE System.init,
// SoundChannel.Play, BGM/video loading, or any other creation of a resampler.
// mix_rate is supplied by AudioServer, not the project setting (a driver may
// negotiate a different rate). Changing the rate of a running world is rejected.
func ovConfigureAudio(cfg map[string]interface{}) error {
	if hostAudioCurrent.Load() != nil {
		return fmt.Errorf("audio is already initialized")
	}
	mode := ""
	if value, present := cfg["audio"]; present {
		var ok bool
		mode, ok = value.(string)
		if !ok {
			return fmt.Errorf("overlay audio must be the string host or sdl")
		}
	}
	if mode == "" || mode == "sdl" {
		ovAudioConfig.host = false
		ovAudioConfig.rate = 0
		hostAudioError.Store(0)
		return nil
	}
	if mode != "host" {
		return fmt.Errorf("unsupported overlay audio mode %q (use host or sdl)", mode)
	}
	var rate float64
	switch v := cfg["mix_rate"].(type) {
	case float64: // encoding/json's default representation.
		rate = v
	case int:
		rate = float64(v)
	case int32:
		rate = float64(v)
	default:
		return fmt.Errorf("audio=host requires AudioServer's integer mix_rate")
	}
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate != math.Trunc(rate) || rate < 8000 || rate > 192000 {
		return fmt.Errorf("host mix_rate must be an integer in 8000..192000 Hz")
	}
	ovAudioConfig.host = true
	ovAudioConfig.rate = beep.SampleRate(rate)
	hostAudioError.Store(0)
	return nil
}

// Shared-file merge hook: replace speaker = &SDLSpeaker{} with this factory.
// Do not normalize/save the rate afterwards: normalize restricts SDL rates to
// 22050/44100/48000, whereas the host may use (for example) 96000 Hz.
func newAudioSink() AudioSink {
	if !ovAudioConfig.host {
		return &SDLSpeaker{}
	}
	sys.cfg.Sound.SampleRate = int32(ovAudioConfig.rate)
	return &HostSpeaker{}
}

// HostSpeaker is the SDL speaker without a device, goroutine, or output queue.
// Play/Lock/Unlock retain beep's existing lock discipline. Mixing owns the same
// mutex, but TryLock makes a host callback return silence instead of waiting
// for a sound trigger. The simulation never waits for the host to consume data.
// As with beep, streamers must not call Play/Lock from inside Stream.
type HostSpeaker struct {
	mu     sync.Mutex
	waiting atomic.Int32 // Lock callers waiting for mu (the mixer gives way to them between blocks)
	mixer  hostmix.Mixer // (beep's Mixer with its working block kept - src/hostmix)
	buf    [hostAudioQuantum][2]float64
	rate   beep.SampleRate
	ready  bool
	paused atomic.Bool
	muted  atomic.Bool
}

var _ AudioSink = (*HostSpeaker)(nil)

func (s *HostSpeaker) Init(rate beep.SampleRate, bufferSize int) error {
	if rate < 8000 || rate > 192000 || bufferSize <= 0 {
		return fmt.Errorf("invalid host audio rate or buffer size")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ready {
		return fmt.Errorf("host audio sink is already initialized")
	}
	s.mixer = hostmix.Mixer{}
	s.rate = rate
	s.ready = true
	s.paused.Store(false)
	s.muted.Store(false)
	if !hostAudioCurrent.CompareAndSwap(nil, s) {
		s.ready = false
		return fmt.Errorf("only one host audio sink may be active")
	}
	return nil
}

func (s *HostSpeaker) Play(st beep.Streamer) {
	if st == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ready {
		s.mixer.Add(st)
	}
}

func (s *HostSpeaker) Lock() {
	if s.mu.TryLock() {
		return
	}
	s.waiting.Add(1)
	s.mu.Lock()
	s.waiting.Add(-1)
}
func (s *HostSpeaker) Unlock()    { s.mu.Unlock() }
func (s *HostSpeaker) FillAudio() {} // Only Godot's audio thread advances the mix.

func (s *HostSpeaker) Close() {
	// A caller already holding s remains safe: Close and Stream use the same mu.
	hostAudioCurrent.CompareAndSwap(s, nil)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ready = false
	s.mixer.Clear()
	clearHostAudioPan(s)
}

// Call these from F's pause/sound command handlers. Pause freezes playback;
// mute keeps consuming samples so unmuting cannot replay an old sound tail.
func ovAudioSetPaused(on bool) {
	if s := hostAudioCurrent.Load(); s != nil {
		s.paused.Store(on)
	}
}

func ovAudioSetMuted(on bool) {
	if s := hostAudioCurrent.Load(); s != nil {
		s.muted.Store(on)
	}
}

// Audio-thread errors are latched as an integer: no formatting, log I/O, or
// acquisition of the overlay/main-thread error mutex from an audio callback.
// F's ikemen_last_error must consult this when its own error buffer is empty.
func ovAudioErrorText() string {
	switch hostAudioError.Load() {
	case hostAudioErrArgument:
		return "ikemen_mix_audio: invalid output buffer, frame count, or mix rate"
	case hostAudioErrRate:
		return "ikemen_mix_audio: host rate changed; restart Ikemen at AudioServer's current mix rate"
	case hostAudioErrPanic:
		return "ikemen_mix_audio: recovered panic in Ikemen's audio mixer"
	default:
		return ""
	}
}

// Returns frames actually consumed, 0 for unavailable/busy/paused, or a negative
// error. The WHOLE destination is initialized to silence, including short reads.
func mixHostAudio(dst []float32, frames, mixRate int) int {
	silenceHostAudio(dst)
	if frames < 0 || frames > hostAudioMaxFrames || len(dst) != frames*2 || mixRate < 8000 || mixRate > 192000 {
		hostAudioError.Store(hostAudioErrArgument)
		return hostAudioErrArgument
	}
	if frames == 0 {
		return 0
	}
	defer ovAudioDumpWrite(dst, frames, mixRate) // (dev, IKEMEN_AUDIO_DUMP: what the host plays, silence included)
	s := hostAudioCurrent.Load()
	if s == nil || s.paused.Load() {
		return 0
	}
	if ovTickLog != nil { // (dev, IKEMEN_TICK_LOG: each mix callback - entered, left, frames)
		enter := ovTimingUs()
		defer func() { ovTickLog.line('A', enter, ovTimingUs(), int64(frames)) }()
	}
	deadline, yieldUntil := time.Time{}, time.Time{}
	lock := func() bool { // (a busy lock waited for, up to hostAudioLockWait this callback)
		if s.mu.TryLock() {
			return true
		}
		if deadline.IsZero() {
			deadline = time.Now().Add(hostAudioLockWait)
		}
		for time.Now().Before(deadline) {
			runtime.Gosched()
			if s.mu.TryLock() {
				return true
			}
		}
		return false
	}
	if !lock() {
		atomic.AddInt64(&simSpeakerBusyMixes, 1) // (dev counter, IKEMEN_SIM_STATS: a callback given silence)
		return 0
	}
	atomic.AddInt64(&simSpeakerMixes, 1)
	locked := true
	defer func() {
		if locked {
			s.mu.Unlock()
		}
	}()
	if !s.ready || s.paused.Load() {
		return 0
	}
	if int(s.rate) != mixRate {
		hostAudioError.Store(hostAudioErrRate)
		return hostAudioErrRate
	}
	consumed := 0
	for consumed < frames {
		if !locked { // (the lock given back after the last block: the Ikemen thread's sound changes went in between)
			if s.waiting.Load() > 0 {
				if yieldUntil.IsZero() {
					yieldUntil = time.Now().Add(hostAudioYield)
				}
				for s.waiting.Load() > 0 && time.Now().Before(yieldUntil) {
					runtime.Gosched()
				}
			}
			if !lock() {
				atomic.AddInt64(&simSpeakerBusyMixes, 1)
				break
			}
			locked = true
			if !s.ready || s.paused.Load() {
				break
			}
		}
		count := frames - consumed
		if count > hostAudioLockBlock {
			count = hostAudioLockBlock
		}
		for i := range s.buf[:count] {
			s.buf[i] = [2]float64{}
		}
		n, _ := s.mixer.Stream(s.buf[:count])
		if n < 0 || n > count {
			panic("audio streamer returned an invalid sample count")
		}
		if !s.muted.Load() && !s.paused.Load() {
			for i := 0; i < n; i++ {
				// Preserve the exact samples SDLSpeaker.QueueAudio would push:
				// clip/truncate to signed 16 bit, then normalize for Godot.
				// Dividing by 32768 is exact in float32 (every PCM bit survives).
				dst[(consumed+i)*2] = float32(floatToS16(s.buf[i][0])) / 32768
				dst[(consumed+i)*2+1] = float32(floatToS16(s.buf[i][1])) / 32768
			}
		}
		consumed += n
		if n < count {
			break
		}
		s.mu.Unlock()
		locked = false
	}
	// A pause/mute arriving during Stream must not leak the block being mixed.
	if s.paused.Load() || s.muted.Load() {
		silenceHostAudio(dst)
	}
	return consumed
}

func silenceHostAudio(dst []float32) {
	for i := range dst {
		dst[i] = 0
	}
}
