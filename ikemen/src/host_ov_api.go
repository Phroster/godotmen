//go:build overlay

package main

// The C ABI of ikemen.dll. Built only with -tags overlay (go build -buildmode=c-shared). Every export recovers its
// own panics: a Go panic must never reach the host.

/*
#include <windows.h>
#include <stdlib.h>
static unsigned long long ikemen_thread_id(void) { return (unsigned long long)GetCurrentThreadId(); }
// Go's Windows monotonic clock in this toolchain reads interrupt time, which
// quantizes sub-millisecond snapshot costs. QPC is diagnostic-only.
static long long ikemen_timing_us(void) {
    LARGE_INTEGER value, frequency;
    QueryPerformanceCounter(&value);
    QueryPerformanceFrequency(&frequency);
    return (long long)((double)value.QuadPart * 1000000.0 / (double)frequency.QuadPart);
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/ikemen-engine/Ikemen-GO/src/hosttid"
	"github.com/veandco/go-sdl2/sdl"
)

const ovBuild = true

func ovCurrentThreadID() uint64 {
	if ovTidFast {
		return hosttid.ID()
	}
	return uint64(C.ikemen_thread_id())
}

// ovTidFast: hosttid.ID (GetCurrentThreadId's own read of the TEB, no cgo transition) gave GetCurrentThreadId's answer
// when the DLL loaded; otherwise every ID stays the cgo call's
var ovTidFast = func() bool {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	fast := hosttid.ID()
	return fast != 0 && fast == uint64(C.ikemen_thread_id())
}()

func ovTimingUs() int64 { return int64(C.ikemen_timing_us()) }

var ovInitDone int32
var ovErrBuf *C.char

func ovRecover(name string, ret *C.int) {
	if r := recover(); r != nil {
		ovSetError("%s: panic: %v", name, r)
		LogMessage("%s", debug.Stack())
		if ret != nil {
			*ret = -1
		}
	}
}

//export ikemen_init
func ikemen_init(ikemenRoot *C.char, configJSON *C.char) (ret C.int) {
	if !atomic.CompareAndSwapInt32(&ovInitDone, 0, 1) {
		ovSetError("ikemen_init: already initialised")
		return -1
	}
	// A failure before the Ikemen thread starts leaves the DLL as it was, so the host may call ikemen_init again
	started := false
	defer func() {
		if !started {
			ovOn = false
			atomic.StoreInt32(&ovInitDone, 0)
		}
	}()
	defer ovRecover("ikemen_init", &ret)
	atomic.StoreInt32(&ovStatus, 0)
	ovErrMu.Lock()
	ovLastErr = ""
	ovErrMu.Unlock()
	ovCfg, ovCfgRaw = ovSettings{}, nil
	root := C.GoString(ikemenRoot)
	js := C.GoString(configJSON)
	// Every relative path is opened from this root (ovPath), not from the process's current folder: a host may switch
	// that folder for moments from its own threads. The chdir stays for anything else.
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	ovRootAbs = filepath.Clean(root)
	if err := os.Chdir(root); err != nil {
		ovFail("ikemen_init: chdir %q: %v", root, err)
		return -1
	}
	if ovLogFile == nil {
		ovOpenLog()
	}
	LogMessage("[overlay] root %s", root)
	LogMessage("[overlay] config %s", js)
	if js != "" {
		if err := json.Unmarshal([]byte(js), &ovCfg); err != nil {
			ovFail("ikemen_init: config_json: %v", err)
			return -1
		}
		json.Unmarshal([]byte(js), &ovCfgRaw)
	}
	if len(ovCfg.Fighters) == 0 && len(ovCfg.Chars) > 0 { // (alias)
		ovCfg.Fighters = ovCfg.Chars
	}
	if os.Getenv("IKEMEN_SOUND") == "1" {
		ovCfg.NoSound = false // (dev: a hidden test with SDL_AUDIODRIVER=dummy runs Ikemen's whole audio path silently)
	} else if os.Getenv("IKEMEN_HIDDEN_RUN") == "1" && ovCfgRaw["audio"] != "host" {
		ovCfg.NoSound = true // (a hidden run is silent, unless the host pulls the mix itself: "audio": "host")
	}
	if len(ovCfg.Fighters) < 2 {
		ovFail("ikemen_init: config_json needs at least 2 fighters (def, pal)")
		return -1
	}
	ovOn = true
	if err := ovParityConfigure(); err != nil {
		ovFail("ikemen_init: parity: %v", err)
		return -1
	}
	// Ikemen's logic is one thread; the Go runtime gets a few more (GC, loader), not one per core inside the host
	procs := ovCfg.GoMaxProcs
	if procs <= 0 || procs > 8 {
		procs = 8 // (config "gomaxprocs": 1..8, default 8)
	}
	if n, err := strconv.Atoi(os.Getenv("IKEMEN_DEV_GOMAXPROCS")); err == nil && n >= 1 && n <= 16 {
		procs = n // (dev: measurements of what more cores would give)
	}
	runtime.GOMAXPROCS(procs)
	// Room for a whole roster's texture uploads queued before the host's first ikemen_gl_pump
	sys.mainThreadTask = make(chan func(), 1<<20)
	if err := ovPrepare(); err != nil {
		ovFail("ikemen_init: %v", err)
		return -1
	}
	ovConfigure(ovCfgRaw)
	ovReplayConfigure(ovCfgRaw) // (replay recording: config replay / replay_mb)
	ovDevSeed()                 // (dev: env IKEMEN_SEED)
	if ovCfg.Pipeline {
		ovWake, ovDone = make(chan struct{}, 1), make(chan struct{}, 1)
	}
	started = true
	go ovThreadMain()
	return 0
}

//export ikemen_gl_init
func ikemen_gl_init() (ret C.int) {
	defer ovRecover("ikemen_gl_init", &ret)
	if !ovOn {
		ovSetError("ikemen_gl_init before ikemen_init")
		return -1
	}
	if ovGLReady {
		return 0
	}
	ovEnterGL()
	defer ovLeaveGL()
	gfx.Init() // (loads the GL entry points from the host's context first)
	ovResetUnpackState()
	gfxFont.Init(gfx)
	ovGLReady = true
	ovDrainTasks()
	return 0
}

//export ikemen_gl_pump
func ikemen_gl_pump() {
	defer ovRecover("ikemen_gl_pump", nil)
	if !ovGLReady {
		return
	}
	ovEnterGL()
	defer ovLeaveGL()
	ovResetUnpackState()
	ovDrainTasks()
}

// The host's pump for frames on which its overlay hook (gl_pump + render) does not run - a minimized window draws
// nothing: the queued GL work (texture uploads and deletes of loading fighters) for at most usec microseconds, so it
// never piles up while the window is hidden and restoring the window never waits on a backlog. Returns the tasks still
// queued (0: drained), -1 on a panic. usec < 0 only counts the queue (no GL call: the host skips its state guard on an
// empty queue). Optional export: a module without it falls back to ikemen_gl_pump.
//
//export ikemen_gl_pump_budget
func ikemen_gl_pump_budget(usec C.int) (ret C.int) {
	defer ovRecover("ikemen_gl_pump_budget", &ret)
	if usec < 0 || !ovGLReady {
		return C.int(len(sys.mainThreadTask))
	}
	ovEnterGL()
	defer ovLeaveGL()
	ovResetUnpackState()
	return C.int(ovDrainTasksFor(time.Duration(usec) * time.Microsecond))
}

//export ikemen_step
func ikemen_step() (ret C.int) {
	defer ovRecover("ikemen_step", &ret)
	return C.int(ovStep())
}

//export ikemen_step_begin
func ikemen_step_begin() (ret C.int) {
	defer ovRecover("ikemen_step_begin", &ret)
	return C.int(ovStepBegin())
}

//export ikemen_step_end
func ikemen_step_end() (ret C.int) {
	defer ovRecover("ikemen_step_end", &ret)
	return C.int(ovStepEnd())
}

//export ikemen_render
func ikemen_render(fbo C.uint, w, h, flags C.int, viewProj16 *C.float, params16 *C.float) (ret C.int) {
	defer ovRecover("ikemen_render", &ret)
	if !ovGLReady {
		ovSetError("ikemen_render before ikemen_gl_init")
		return -1
	}
	if ovTickLog != nil { // (dev: IKEMEN_TICK_LOG - entered, GL tasks drained, left)
		enter := ovTimingUs()
		defer func() { ovTickLog.line('R', enter, ovTickDrained, ovTimingUs(), int64(atomic.LoadUint64(&ovTicks))) }()
	}
	ovEnterGL()
	defer ovLeaveGL()
	ovResetUnpackState()
	ovDrainTasks()
	if ovTickLog != nil {
		ovTickDrained = ovTimingUs()
	}
	if atomic.LoadInt32(&ovStatus) != 1 {
		return 0 // booting (the Ikemen thread is running, not parked) or failed: nothing to draw
	}
	var vp, params [16]float32
	if viewProj16 != nil {
		vp = *(*[16]float32)(unsafe.Pointer(viewProj16))
	}
	if params16 != nil {
		params = *(*[16]float32)(unsafe.Pointer(params16))
	}
	return C.int(sys.ovRenderFrame(uint32(fbo), int32(w), int32(h), int(flags), vp, params))
}

//export ikemen_render_live
func ikemen_render_live(fbo C.uint, w, h, flags C.int, viewProj16 *C.float, params16 *C.float) (ret C.int) {
	defer ovRecover("ikemen_render_live", &ret)
	if !ovGLReady || ovStepPending || atomic.LoadInt32(&ovStatus) != 1 {
		ovSetError("render_live requires a ready parked world and GL context")
		return -1
	}
	ovEnterGL()
	defer ovLeaveGL()
	ovResetUnpackState()
	ovDrainTasks()
	var vp, params [16]float32
	if viewProj16 != nil {
		vp = *(*[16]float32)(unsafe.Pointer(viewProj16))
	}
	if params16 != nil {
		params = *(*[16]float32)(unsafe.Pointer(params16))
	}
	return C.int(sys.ovRenderLiveFrame(uint32(fbo), int32(w), int32(h), int(flags), vp, params))
}

//export ikemen_pipeline_stats
func ikemen_pipeline_stats(buf *C.float, capFloats C.int) (ret C.int) {
	defer ovRecover("ikemen_pipeline_stats", &ret)
	if buf == nil || capFloats < 5 || !ovCfg.Pipeline {
		return 0
	}
	ovSnapshots.RLock()
	defer ovSnapshots.RUnlock()
	f := ovSnapshots.latest
	if f == nil {
		return 0
	}
	out := unsafe.Slice((*float32)(unsafe.Pointer(buf)), int(capFloats))
	out[0], out[1], out[2], out[3], out[4] = float32(f.tick), float32(f.simulationUs)/1000,
		float32(f.buildUs)/1000, float32(len(f.ops)), float32(cap(f.ops))*float32(unsafe.Sizeof(ovDrawOp{}))
	return 5
}

//export ikemen_command
func ikemen_command(js *C.char) (ret C.int) {
	defer ovRecover("ikemen_command", &ret)
	if ovCfg.Pipeline && ovStepPending && ovStepEnd() < 0 {
		return -1
	}
	b := []byte(C.GoString(js))
	var head struct {
		Op string `json:"op"`
		On *bool  `json:"on"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		ovSetError("ikemen_command: %v", err)
		return -1
	}
	switch head.Op {
	case "pause":
		on := head.On == nil || *head.On
		atomic.StoreInt32(&ovPaused, int32(Btoi(on)))
		ovApplyAudioPause()
		return 0
	case "sound":
		on := head.On == nil || *head.On
		atomic.StoreInt32(&ovMuted, int32(Btoi(!on)))
		ovAudioSetMuted(!on)
		return 0
	}
	var n int
	var err error
	if ovReplayOp(head.Op) {
		_, n, err = ovReplayCommand(b) // (clip_* / replay_* ops - picked by name, no second parse for the others)
	} else {
		n, err = ovCommand(b)
	}
	if err != nil {
		ovSetError("ikemen_command %s: %v", head.Op, err)
		if n >= 0 {
			n = -1
		}
	}
	return C.int(n)
}

//export ikemen_state
func ikemen_state(buf *C.float, capFloats C.int) (ret C.int) {
	defer ovRecover("ikemen_state", &ret)
	if buf == nil || capFloats <= 0 || atomic.LoadInt32(&ovStatus) != 1 {
		return 0
	}
	out := unsafe.Slice((*float32)(unsafe.Pointer(buf)), int(capFloats))
	if ovCfg.Pipeline {
		if !ovStepPending {
			ovCopyJoinedState()
		}
		n := Min(len(out)/ovREC*ovREC, ovJoinedStateN)
		copy(out, ovJoinedState[:n])
		return C.int(n)
	}
	return C.int(ovFillState(out))
}

//export ikemen_events
func ikemen_events(buf *C.char, capBytes C.int) (ret C.int) {
	defer ovRecover("ikemen_events", &ret)
	if ovCfg.Pipeline && ovStepPending && ovStepEnd() < 0 {
		return -1
	}
	if buf == nil || capBytes <= 0 {
		return 0
	}
	out := unsafe.Slice((*byte)(unsafe.Pointer(buf)), int(capBytes))
	n := ovDrainEvents(out)
	if n < 0 {
		n = 0
	}
	n += ovDrainFrameEvents(out[n:])
	return C.int(n)
}

//export ikemen_last_error
func ikemen_last_error() *C.char {
	defer ovRecover("ikemen_last_error", nil)
	old := ovErrBuf
	message := ovLastError()
	if message == "" {
		message = ovAudioErrorText()
	}
	ovErrBuf = C.CString(message)
	if old != nil {
		C.free(unsafe.Pointer(old))
	}
	return ovErrBuf
}

//export ikemen_status
func ikemen_status() C.int {
	return C.int(atomic.LoadInt32(&ovStatus))
}

//export ikemen_shutdown
func ikemen_shutdown() {
	defer ovRecover("ikemen_shutdown", nil)
	if !ovOn {
		return
	}
	if ovCfg.Pipeline && ovStepPending {
		ovStepEnd()
	}
	if ovCfg.ParityMode {
		ovParityShutdown()
	}
	first := false
	ovQuitOnce.Do(func() { close(ovQuit); first = true })
	if s := hostAudioCurrent.Load(); s != nil {
		s.Close() // withdraw the host callback's sink, synchronise any current mix
	}
	if !first {
		return
	}
	// Everything parked before the host tears its process down: the Ikemen thread ends its match and returns (it
	// is parked at a frame boundary or spinning in the loader's await, both watch ovQuit), then the audio pusher stops
	// and SDL's audio device and thread are closed - nothing of the DLL runs while the host's CRT and DLLs unload.
	t0 := time.Now()
	ended := false
	select {
	case <-ovDead:
		ended = true
	case <-time.After(3 * time.Second):
	}
	ovCloseAudio()
	ovPprofStop()
	ovRenderStatsTotal()
	atomic.StoreInt32(&ovStatus, -1) // (any later call is a no-op: ikemen_step fails, ikemen_render draws nothing)
	LogMessage("[overlay] shutdown after %d ticks: Ikemen thread %s in %.0f ms, audio closed, %d huge sprite draws cut",
		ovTicks, map[bool]string{true: "ended", false: "STILL RUNNING"}[ended],
		float64(time.Since(t0).Microseconds())/1000, ovHugeCuts)
	LogMessage("[overlay] render thread totals: %d decodes (%.1f ms), %d uploads (%d MB), %d renders >= 5 ms",
		ovTotalDecodes, float64(ovTotalDecodeUs)/1000, ovTotalUploads, ovTotalUploadBytes>>20, ovSlowRenders)
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	LogMessage("[overlay] Go heap at shutdown: %d MB in use, %d MB from the OS, %d GCs, %d textures / %d MB uploaded",
		ms.HeapInuse>>20, ms.Sys>>20, ms.NumGC, atomic.LoadInt64(&ovTexCount), atomic.LoadInt64(&ovTexBytes)>>20)
	if ovLogFile != nil {
		ovLogFile.Sync()
	}
}

// Stops the audio pusher goroutine and closes Ikemen's SDL audio device and subsystem (ikemen_shutdown)
func ovCloseAudio() {
	ovAudioMu.Lock()
	defer ovAudioMu.Unlock()
	if sp, ok := speaker.(*SDLSpeaker); ok && sp.dev != 0 {
		sdl.PauseAudioDevice(sp.dev, true)
		sdl.CloseAudioDevice(sp.dev)
		sp.dev = 0
	}
	if sdl.WasInit(sdl.INIT_AUDIO) != 0 {
		sdl.QuitSubSystem(sdl.INIT_AUDIO)
	}
}

// Pauses Ikemen's own audio device with the world (host pause menu)
func ovApplyAudioPause() {
	ovAudioSetPaused(atomic.LoadInt32(&ovPaused) != 0)
	if sp, ok := speaker.(*SDLSpeaker); ok && sp.dev != 0 {
		sdl.PauseAudioDevice(sp.dev, atomic.LoadInt32(&ovPaused) != 0)
	}
}

var _ = fmt.Sprintf
