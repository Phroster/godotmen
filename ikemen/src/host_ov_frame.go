package main

// The overlay: Ikemen GO runs inside a host (a game engine) as ikemen.dll and draws every fighter itself straight into
// the host's frame. This file holds the overlay mode's runtime state:
//   - the host rendezvous (System.await parks the Ikemen thread; ikemen_step wakes it for exactly one tick),
//   - the GL-thread rule (GL work only on the host's thread, inside ikemen_gl_init / ikemen_gl_pump / ikemen_render;
//     anything else is queued on sys.mainThreadTask),
//   - the 3D "card" projection (every sprite drawn on a vertical, camera-facing plane at its owner's depth),
//   - the overlay frame (System.draw with the overlay guards, into the host's FBO).
// It is compiled into every build; ovOn stays false unless ikemen_init (host_ov_api.go, build tag "overlay") runs,
// so a stand-alone build behaves exactly as stock Ikemen GO.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	mgl "github.com/go-gl/mathgl/mgl32"
	"github.com/go-gl/mathgl/mgl64"
	lua "github.com/yuin/gopher-lua"
)

// Render flags of ikemen_render (bits 16, 32 and 64 are reserved)
const (
	ovFlagFlipY     = 1   // the target is stored bottom-up: clip-space y is flipped
	ovFlagNoClear   = 2   // draw onto the pixels already there
	ovFlagDepthTest = 4   // test against the host's scene depth, no depth writes
	ovFlagDebugClsn = 8   // draw Ikemen's collision boxes
	ovFlagLatePass  = 128 // drawn before the host's transparent pass (host_ov_depth.go)
)

// Overlay mode on (set once by ikemen_init before the Ikemen thread starts; never cleared)
var ovOn bool

type ovFighterCfg struct {
	Def string `json:"def"`
	Pal int    `json:"pal"`
	Ai  int    `json:"ai"`
}

// What ikemen_init's config_json may hold (unknown keys go to ovConfigure untouched)
type ovSettings struct {
	Pipeline       bool           `json:"pipeline"`    // immutable draw snapshots + split host tick (opt-in)
	ParityMode     bool           `json:"parity_mode"` // default off: the renderer test profile (stock match, flat 2D)
	ParityScenario string         `json:"parity_scenario"`
	ParityOutput   string         `json:"parity_output"`
	Fighters       []ovFighterCfg `json:"fighters"`
	Chars          []ovFighterCfg `json:"chars"` // alias of fighters
	Stage          string         `json:"stage"`
	NoSound        bool           `json:"nosound"`
	NoMusic        *bool          `json:"nomusic"`
	IkLocalcoord   float32        `json:"ik_localcoord"` // the localcoord width whose pixel is one ik unit (default 320)
	Width          int32          `json:"width"`         // Ikemen's virtual screen (only scales the 2D maths)
	Height         int32          `json:"height"`
	RoundTime      int            `json:"round_time"` // -1 = no round timer
	MatchWins      int            `json:"match_wins"`
	SpriteShake    bool           `json:"sprite_shake"`   // keep Ikemen's EnvShake offset on sprites (default off: host shakes)
	TickTimeout    float64        `json:"tick_timeout"`   // seconds ikemen_step waits for one tick before failing
	GoMaxProcs     int            `json:"gomaxprocs"`     // Go runtime threads running Go code at once (default 8, 1-8)
	HugeCut        *bool          `json:"huge_cut"`       // screen-filling effect sprites are not drawn (default off: ovHugeCut)
	FloorRule      *bool          `json:"floor_rule"`     // what lies below a ground line is not cut by the host's floor (default on)
	Smooth         *bool          `json:"smooth"`         // texel edges antialiased on a perspective camera (default on)
	ScreenSprites  *bool          `json:"screen_sprites"` // screen-space sprites drawn flat over the frame (default on)
}

var ovCfg ovSettings
var ovCfgRaw map[string]interface{}

// ---------------------------------------------------------------------------------------------------------------------
// Paths: every relative path Ikemen opens is resolved from the Ikemen root, never from the process's current folder.
// A host may change the process-wide current folder at any time (stock Godot's DirAccess does on Windows, on every
// open), so a loader goroutine opening "data/common.const" in that moment would not find it, and an os.Chdir of
// Ikemen's own could be undone by the host. ovRootAbs is set by ikemen_init; empty (a stand-alone build) leaves every
// path exactly as given.

var ovRootAbs string

// ovPath: a relative file name anchored at the Ikemen root (absolute, drive- or root-relative names unchanged). Only
// the file system call uses it: the name Ikemen keeps (caches, logs, a pack's own references) stays as written.
func ovPath(name string) string {
	if ovRootAbs == "" || name == "" || filepath.IsAbs(name) || filepath.VolumeName(name) != "" ||
		name[0] == '/' || name[0] == '\\' {
		return name
	}
	return filepath.Join(ovRootAbs, name)
}

// ovGlob: filepath.Glob of a relative pattern from the Ikemen root, the matches given back relative as Glob would
func ovGlob(pattern string) ([]string, error) {
	if ovPath(pattern) == pattern {
		return filepath.Glob(pattern)
	}
	var root strings.Builder // (the root itself literal: no escapes on Windows, a class per meta character instead)
	for _, r := range filepath.ToSlash(ovRootAbs) {
		if r == '[' || r == '*' || r == '?' {
			root.WriteString("[" + string(r) + "]")
		} else {
			root.WriteRune(r)
		}
	}
	m, err := filepath.Glob(root.String() + "/" + pattern)
	for i := range m {
		if rel, e := filepath.Rel(ovRootAbs, m[i]); e == nil {
			m[i] = rel
		}
	}
	return m, err
}

// ---------------------------------------------------------------------------------------------------------------------
// Status, errors, log

var ovStatus int32 // 0 booting, 1 ready (match running), -1 failed
var ovErrMu sync.Mutex
var ovLastErr string
var ovLogFile *os.File

func ovSetError(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	ovErrMu.Lock()
	ovLastErr = msg
	ovErrMu.Unlock()
	LogMessage("[overlay] error: %s", msg)
	ovPushEvent(map[string]interface{}{"e": "error", "msg": msg})
}

func ovFail(format string, a ...any) {
	ovSetError(format, a...)
	atomic.StoreInt32(&ovStatus, -1)
}

func ovLastError() string {
	ovErrMu.Lock()
	defer ovErrMu.Unlock()
	return ovLastErr
}

// ovLogPath: this run's overlay log - env IKEMEN_OV_LOG=<file> (for runs side by side on one root, which would
// otherwise truncate each other's log), else <root>/save/logs/overlay.log
func ovLogPath() string {
	if p := os.Getenv("IKEMEN_OV_LOG"); p != "" {
		return p
	}
	return ovPath(filepath.Join("save", "logs", "overlay.log"))
}

// Opens the overlay log (ovLogPath) and points Ikemen's log (os.Stderr / os.Stdout) at it
func ovOpenLog() {
	os.MkdirAll(filepath.Dir(ovLogPath()), 0755)
	if os.Getenv("IKEMEN_OV_LOG") == "" {
		// (the last session's kept as overlay.1.log: when the process dies inside Go - a fatal error the host never
		// sees - the cause would otherwise be cut off by the next start's truncate)
		os.Rename(ovLogPath(), strings.TrimSuffix(ovLogPath(), ".log")+".1.log")
	}
	if f, err := os.OpenFile(ovLogPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_APPEND, 0644); err == nil {
		// (append mode: the core's own log handle writes into the same file)
		ovLogFile = f
		os.Stderr = f
		os.Stdout = f
	}
	LogMessage("[overlay] %s ikemen.dll %s (%s), pid %d", time.Now().Format("2006-01-02 15:04:05"), Version, BuildTime, os.Getpid())
}

// Frame-side events (rumble, envcolor) appended to C's event lines by ikemen_events
var ovEventMu sync.Mutex
var ovEvents []byte

func ovPushEvent(ev map[string]interface{}) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	ovEventMu.Lock()
	if len(ovEvents) < 1<<20 {
		ovEvents = append(ovEvents, b...)
		ovEvents = append(ovEvents, '\n')
	}
	ovEventMu.Unlock()
}

// Copies whole lines of frame events into buf; returns bytes written
func ovDrainFrameEvents(buf []byte) int {
	ovEventMu.Lock()
	defer ovEventMu.Unlock()
	n := 0
	for n < len(ovEvents) {
		end := n
		for end < len(ovEvents) && ovEvents[end] != '\n' {
			end++
		}
		if end >= len(ovEvents) || end+1 > len(buf) {
			break
		}
		end++
		n = end
	}
	copy(buf, ovEvents[:n])
	ovEvents = append(ovEvents[:0], ovEvents[n:]...)
	return n
}

// ---------------------------------------------------------------------------------------------------------------------
// GL thread rule

var ovGLThread uint64 // OS thread id of the host thread inside an host GL call
var ovGLActive int32  // 1 while the host thread is inside ikemen_gl_init / ikemen_gl_pump / ikemen_render
var ovGLReady bool    // gfx.Init done on the host's context
var ovTexCount int64  // textures created by the overlay (GL thread only)
var ovTexBytes int64  // their texel bytes

func ovEnterGL() {
	atomic.StoreUint64(&ovGLThread, ovCurrentThreadID())
	atomic.StoreInt32(&ovGLActive, 1)
}

func ovLeaveGL() {
	atomic.StoreInt32(&ovGLActive, 0)
}

// True on the host's thread inside an host GL call (the only place GL may be called in overlay mode)
func ovOnGLThread() bool {
	return atomic.LoadInt32(&ovGLActive) == 1 && atomic.LoadUint64(&ovGLThread) == ovCurrentThreadID()
}

// Overlay mode and not on the GL thread: GL work must go to sys.mainThreadTask instead
func ovMustDeferGL() bool {
	return ovOn && !ovOnGLThread()
}

// Runs the queued GL work (texture uploads, deletes) on the host's thread. Each task is recovered on its own: a
// goroutine panic forwarded by SafeGo fails the overlay instead of killing the host.
func ovDrainTasks() {
	for {
		select {
		case f := <-sys.mainThreadTask:
			ovRunTask(f)
		default:
			return
		}
	}
}

// ovDrainTasks for at most budget (ikemen_gl_pump_budget: the host's pump on frames its overlay hook does not run, e.g.
// while the window is minimized): stops after the task that crosses the budget. Returns the tasks still queued.
func ovDrainTasksFor(budget time.Duration) int {
	deadline := time.Now().Add(budget)
	for {
		select {
		case f := <-sys.mainThreadTask:
			ovRunTask(f)
			if time.Now().After(deadline) {
				return len(sys.mainThreadTask)
			}
		default:
			return 0
		}
	}
}

func ovRunTask(f func()) {
	defer func() {
		if r := recover(); r != nil {
			ovFail("Ikemen goroutine panic: %v", r)
			LogMessage("%s", debug.Stack())
		}
	}()
	f()
}

// ---------------------------------------------------------------------------------------------------------------------
// Rendezvous: the Ikemen thread parks at every frame boundary of the match (System.await called from update());
// ikemen_step wakes it for exactly one loop iteration (one 60 Hz tick: logic + cueDraw) and waits until it parks again.

var ovWake = make(chan struct{})
var ovDone = make(chan struct{})
var ovDead = make(chan struct{})
var ovQuit = make(chan struct{})
var ovQuitOnce sync.Once
var ovParkedOnce bool
var ovPaused int32
var ovMuted int32
var ovTicks uint64

// Overlay replacement of System.await. fromUpdate = the call comes from System.update() (the match loop's frame
// boundary); every other await (the loader's spin in game(), Lua) only yields while the match is booting.
func (s *System) ovAwait(fromUpdate bool) bool {
	s.frameSkip = false // never skip: cueDraw records afterimages, DrawList.add drops sprites on skipped frames
	if !fromUpdate || (ovCfg.ParityMode && !s.gameRunning) {
		// Stock quick-VS refreshes its loading screen before game(). That is
		// a boot yield, never a match boundary or a ready fighter state.
		select {
		case <-ovQuit:
			s.gameEnd = true
		default:
			time.Sleep(time.Millisecond)
		}
		return !s.gameEnd
	}
	firstPark := !ovParkedOnce
	if firstPark {
		ovParkedOnce = true
		ovBootSetup()
		ovAudioSnapshotPan()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		if len(sys.cgi) > 0 {
			ovPreloadPacks(sys.cgi[0].def) // (every pack's fight fx in the background from here)
		}
		LogMessage("[overlay] match running: first frame boundary reached (Go heap %d MB in use, %d MB from the OS, %d GL tasks queued, %d textures / %d MB uploaded so far)",
			ms.HeapInuse>>20, ms.Sys>>20, len(sys.mainThreadTask), atomic.LoadInt64(&ovTexCount), atomic.LoadInt64(&ovTexBytes)>>20)
	} else {
		ovAudioSnapshotPan()
		if ovCfg.Pipeline {
			s.ovScreenEvents()
			p0 := ovTimingUs()
			ovPublishDrawSnapshot(s)
			if simStats != nil && ovSimulationStarted != 0 { // (dev: IKEMEN_SIM_STATS - the pipelined tick, wake to done)
				p1 := ovTimingUs()
				simStats.tickDone(p1-ovSimulationStarted, p1-p0)
			}
			if ovTickLog != nil && ovSimulationStarted != 0 { // (dev: IKEMEN_TICK_LOG)
				ovTickLog.tick(ovSimulationStarted, ovTickBetween, p0, ovTimingUs())
			}
		}
		select {
		case ovDone <- struct{}{}:
		case <-ovQuit:
			s.gameEnd = true
			return false
		}
	}
	if !ovCfg.Pipeline || firstPark {
		s.ovScreenEvents()
	}
	if atomic.LoadUint64(&ovTicks)%600 == 300 { // (every 10 s)
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		LogMessage("[overlay] tick %d: Go heap %d MB in use, %d MB from the OS, %d textures / %d MB uploaded",
			atomic.LoadUint64(&ovTicks), ms.HeapInuse>>20, ms.Sys>>20, atomic.LoadInt64(&ovTexCount), atomic.LoadInt64(&ovTexBytes)>>20)
	}
	if firstPark {
		if ovCfg.Pipeline {
			ovPublishDrawSnapshot(s)
		}
		atomic.StoreInt32(&ovStatus, 1) // (only now: from here on this thread touches nothing until woken)
	}
	select {
	case <-ovWake:
		if ovCfg.Pipeline {
			ovSimulationStarted = ovTimingUs()
			s.clsnDisplay = ovSnapshotDebug.Load() != 0
		}
	case <-ovQuit:
		s.gameEnd = true
		return false
	}
	ovBetweenTicks()
	if simStats != nil {
		simStats.betweenDone = ovTimingUs() // (IKEMEN_SIM_STATS slow ticks)
	}
	if ovTickLog != nil {
		ovTickBetween = ovTimingUs() // (IKEMEN_TICK_LOG)
	}
	return !s.gameEnd
}

// Whole-screen effects the overlay does not draw, reported to the host once when they start (Ikemen thread, after
// each tick): EnvColor (the fill is not drawn and layer-0 sprites stay visible) and EnvShake (sprites do not shake
// unless sprite_shake; the host shakes its camera). C's own "shake" events (per attacker) may come as well.
var ovPrevEnvcol [4]int32
var ovPrevShakeTime int32

func (s *System) ovScreenEvents() {
	ec := [4]int32{s.envcol[0], s.envcol[1], s.envcol[2], s.envcol_time}
	if s.envcol_time != 0 && (ovPrevEnvcol[3] == 0 || ec[0] != ovPrevEnvcol[0] || ec[1] != ovPrevEnvcol[1] ||
		ec[2] != ovPrevEnvcol[2] || s.envcol_time > ovPrevEnvcol[3]) {
		ovPushEvent(map[string]interface{}{"e": "envcolor", "rgb": []int32{ec[0], ec[1], ec[2]},
			"time": s.envcol_time, "under": s.envcol_under})
	}
	ovPrevEnvcol = ec
	es := &s.envShake
	if es.time > 0 && es.time > ovPrevShakeTime {
		ovPushEvent(map[string]interface{}{"e": "envshake", "time": es.time, "ampl": es.ampl, "freq": es.freq,
			"dir": es.dir})
	}
	ovPrevShakeTime = es.time
}

// One host tick. Returns 1 ticked, 0 not ready yet (booting) or paused, -1 failed.
func ovStepLegacy() int {
	switch atomic.LoadInt32(&ovStatus) {
	case 0:
		return 0
	case -1:
		return -1
	}
	if atomic.LoadInt32(&ovPaused) != 0 || ovReplayIsPlaying() { // (the live world holds while a clip plays)
		return 0
	}
	timeout := time.Duration(ovCfg.TickTimeout * float64(time.Second))
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case ovWake <- struct{}{}:
	case <-ovDead:
		ovFail("Ikemen thread ended")
		return -1
	case <-timer.C:
		ovFail("Ikemen thread did not take the tick within %v", timeout)
		return -1
	}
	select {
	case <-ovDone:
	case <-ovDead:
		if atomic.LoadInt32(&ovStatus) != -1 {
			ovFail("Ikemen thread ended during a tick")
		}
		return -1
	case <-timer.C:
		ovFail("one tick took longer than %v", timeout)
		return -1
	}
	atomic.AddUint64(&ovTicks, 1)
	ovReplayCapture() // (this tick's cued draw list and accepted sounds, while the Ikemen thread is parked)
	return 1
}

// ---------------------------------------------------------------------------------------------------------------------
// Boot: realMain for the overlay (no command line, no window, no GL, no stdin, no os.Exit)

// Runs inside ikemen_init on the host's thread: configuration and the renderer object (no GL yet), so ikemen_gl_init can
// run as soon as ikemen_init returns
func ovPrepare() error {
	sys.baseDir = "./"
	os.MkdirAll(ovPath("save/replays"), 0755)
	os.MkdirAll(ovPath("save/logs"), 0755)

	sys.cmdFlags = map[string]string{"-windowed": "true", "-nojoy": "true"}
	if ovCfg.NoMusic == nil || *ovCfg.NoMusic {
		sys.cmdFlags["-nomusic"] = "true"
	}
	if ovCfg.NoSound {
		sys.cmdFlags["-nosound"] = "true"
	}
	sys.cmdFlags["-stats"] = ovPath(filepath.Join(sys.baseDir, "save/stats.json"))
	if ovCfg.ParityMode {
		// the stock quick-VS command line: the boot fighters, their palettes and AI levels, the stage
		for pn, f := range ovCfg.Fighters {
			key := fmt.Sprintf("-p%d", pn+1)
			sys.cmdFlags[key] = strings.TrimPrefix(filepath.ToSlash(f.Def), "chars/")
			sys.cmdFlags[key+".pal"] = fmt.Sprint(f.Pal)
			sys.cmdFlags[key+".ai"] = fmt.Sprint(f.Ai)
		}
		sys.cmdFlags["-s"] = ovCfg.Stage
	}
	if _, err := os.ReadFile(sys.cmdFlags["-stats"]); err != nil {
		os.WriteFile(sys.cmdFlags["-stats"], []byte("{}"), 0644)
	}

	initLUTs()
	// (read only: stock loadConfig writes the file back at every boot, truncate + write - two overlay boots at once
	// could read it half-written, fall back to Ikemen's stock defaults and save those over it. The overlay's settings
	// are applied in memory: ovApplyConfig)
	ovConfigReadOnly = true
	cfg, err := loadConfig("save/config.ini")
	if err != nil {
		return fmt.Errorf("loadConfig: %w", err)
	}
	sys.cfg = *cfg
	ovApplyConfig()
	if err := ovConfigureAudio(ovCfgRaw); err != nil {
		return fmt.Errorf("overlay audio: %w", err)
	}

	if _, err := os.Stat(ovPath(sys.cfg.Config.System)); err != nil {
		return fmt.Errorf("overlay boot script: %w", err)
	}
	gfx, gfxFont = selectRenderer(sys.cfg.Video.RenderMode)
	if _, ok := gfx.(*Renderer_GL33); !ok {
		return fmt.Errorf("overlay needs the OpenGL 3.3 renderer, got %s", gfx.GetName())
	}
	return nil
}

// The Ikemen thread: everything else of realMain - System.init (no window, no GL), the boot script, the match
func ovThreadMain() {
	runtime.LockOSThread() // SDL audio and Lua stay on this thread; never unlocked
	defer close(ovDead)
	defer func() {
		if r := recover(); r != nil {
			ovFail("Ikemen thread panic: %v", r)
			LogMessage("%s", debug.Stack())
		}
	}()

	sys.luaLState = sys.init(sys.cfg.Video.GameWidth, sys.cfg.Video.GameHeight)
	ovSetLuaConfig(sys.luaLState)
	LogMessage("[overlay] boot script %s", sys.cfg.Config.System)
	if err := sys.luaLState.DoFile(ovPath(sys.cfg.Config.System)); err != nil {
		if !strings.Contains(err.Error(), "<game end>") {
			panic(err)
		}
	}
	select {
	case <-ovQuit:
		LogMessage("[overlay] Ikemen thread ended by ikemen_shutdown")
		atomic.StoreInt32(&ovStatus, -1)
	default:
		if atomic.LoadInt32(&ovStatus) != -1 {
			ovFail("Ikemen's match ended (overlay.lua returned)")
		}
	}
}

// ovConfigReadOnly: (overlay boot) loadConfig does not write save/config.ini back
var ovConfigReadOnly bool

// The overlay's fixed engine settings, whatever save/config.ini says
func ovApplyConfig() {
	c := &sys.cfg
	ovHugeOn = ovCfg.HugeCut != nil && *ovCfg.HugeCut
	ovFloorRuleOn = ovCfg.FloorRule == nil || *ovCfg.FloorRule
	ovSmoothOn = ovCfg.Smooth == nil || *ovCfg.Smooth
	ovScreenSpritesOn = ovCfg.ScreenSprites == nil || *ovCfg.ScreenSprites
	if ovCfg.ParityMode {
		// Keep the reference runtime's native settings and quick-VS Lua boot.
		// The existing os.exit wrapper in ovSetLuaConfig protects the host.
		c.Config.System = "external/script/main.lua"
		ovCfg.IkLocalcoord = 320
		ovCfg.SpriteShake = true
		return
	}
	c.Config.System = "external/script/overlay.lua"
	c.Video.RenderMode = "OpenGL 3.3"
	c.Video.MSAA = 0
	c.Video.VSync = 0
	c.Video.Fullscreen = false
	c.Video.RendererDebugMode = false
	c.Video.ExternalShaders = nil
	c.Video.EnableModel = false
	c.Video.EnableModelShadow = false
	c.Video.KeepAspect = false
	c.Video.Framerate = 60 // one host tick = one logic tick = one cued frame (no interpolation, no catch-up)
	if ovCfg.Width > 0 && ovCfg.Height > 0 {
		c.Video.GameWidth, c.Video.GameHeight = ovCfg.Width, ovCfg.Height
	} else {
		c.Video.GameWidth, c.Video.GameHeight = 1280, 960 // (only the 2D maths: nothing is drawn at this size)
	}
	c.Config.WindowIcon = nil
	sys.msaa = 0
	LogMessage("[overlay] huge-sprite cut %v, floor rule %v, smooth %v, screen sprites %v", ovHugeOn, ovFloorRuleOn,
		ovSmoothOn, ovScreenSpritesOn)
	if ovCfg.IkLocalcoord <= 0 {
		ovCfg.IkLocalcoord = 320
	}
}

// Lua global hostOverlay = {fighters = {{def, pal, ai}, ...}, stage, roundTime, matchWins} for overlay.lua
func ovSetLuaConfig(l *lua.LState) {
	t := l.NewTable()
	fighters := l.NewTable()
	for _, f := range ovCfg.Fighters {
		ft := l.NewTable()
		ft.RawSetString("def", lua.LString(filepath.ToSlash(f.Def)))
		pal := f.Pal
		if pal < 1 {
			pal = 1
		}
		ft.RawSetString("pal", lua.LNumber(pal))
		ft.RawSetString("ai", lua.LNumber(f.Ai))
		fighters.Append(ft)
	}
	t.RawSetString("fighters", fighters)
	t.RawSetString("stage", lua.LString(filepath.ToSlash(ovCfg.Stage)))
	rt := ovCfg.RoundTime
	if rt == 0 {
		rt = -1
	}
	t.RawSetString("roundTime", lua.LNumber(rt))
	mw := ovCfg.MatchWins
	if mw <= 0 {
		mw = 99
	}
	t.RawSetString("matchWins", lua.LNumber(mw))
	l.SetGlobal("hostOverlay", t)
	// Lua's os.exit would end the host's process: it ends the boot script instead
	if osTable, ok := l.GetGlobal("os").(*lua.LTable); ok {
		osTable.RawSetString("exit", l.NewFunction(func(l *lua.LState) int {
			l.RaiseError("<game end>")
			return 0
		}))
	}
}

// ---------------------------------------------------------------------------------------------------------------------
// Sprite owner for the card projection: set by System.cueDraw / explodCueDraw around each owner's cueDraw,
// copied into every SpriteData by newSpriteData (afterimages keep the depth they were recorded at).

var ovCueZ, ovCueX float32
var ovCueY float32         // the anchor's height in ik units, Ikemen's sign (+ = below the ground line): the floor rule
var ovCueAnchorY float32   // the anchor's height the card is placed with (world-vertical motion)
var ovCueRoot *Char        // the owner's root fighter (huge-sprite option); nil outside System.cueDraw
var ovCueKind int8         // 0 the root's own body, 1 helper, 2 explod, 3 projectile
var ovCueAnimNo int32      // the owner's action number
var ovCueExplodOf *Explod  // the explod being cued (nil for chars and projectiles)
var ovCueRootZ = ovNoDepth // the owner's root fighter's depth (ik units; NaN: no root): the depth order's

var ovNoDepth = float32(math.NaN())

// The root fighter of player slot pn (nil when the slot is empty)
func ovRootOf(pn int) *Char {
	if pn >= 0 && pn < len(sys.chars) && len(sys.chars[pn]) > 0 {
		return sys.chars[pn][0]
	}
	return nil
}

// After System.cueDraw: sprites cued by anything else (fight screen) have no owner fighter
func ovCueEnd() {
	ovCueRoot, ovCueKind, ovCueAnimNo, ovCueExplodOf = nil, 0, 0, nil
	ovCueAnchorY = 0
	ovCueRootZ = ovNoDepth
}

// ik units of a value in a char's local units. One ik unit is one pixel of a fighter whose localcoord width is the
// init config's "ik_localcoord".
func ovIkUnits(v, localscl float32) float32 {
	if localscl == 0 {
		return v
	}
	gu := sys.gameWidth / ovCfg.IkLocalcoord // game units per ik unit
	if gu == 0 {
		return v
	}
	return v * localscl / gu
}

func ovCueChar(c *Char) { ovCueCharFor(c, true) }

// ovCueCharFor: own false - the char is an explod's or projectile's maker (ovCueExplod, ovCueProjectile), which keep
// only its depth (ovCueZ, ovCueRootZ) and set every other cue field themselves
func ovCueCharFor(c *Char, own bool) {
	if c == nil {
		return
	}
	var v ovCueVals
	ovCueCharTo(&v, c, own)
	v.set()
}

// ovCueCharTo: ovCueCharFor's values into v (c not nil; every field of v set) - nothing global written, so the same
// code serves the cue's globals and the explods' parallel cue (host_ov_cue_par.go)
func ovCueCharTo(v *ovCueVals, c *Char, own bool) {
	v.Z = ovSpriteZ(c)
	v.X = ovIkUnits(c.interPos[0], c.localscl)
	v.Y = ovIkUnits(c.interPos[1], c.localscl)
	v.AnchorY = v.Y
	v.Root = ovRootOf(c.playerNo)
	v.RootZ = ovNoDepth
	if v.Root != nil {
		v.RootZ = ovSpriteZ(v.Root)
	}
	v.Kind = 0
	if c.helperIndex != 0 {
		v.Kind = 1
	}
	v.AnimNo = c.animNo
	v.ExplodOf = nil
}

// An explod's card stands at its OWN depth when it has one: placed at a fighter (postype p1/p2, stage space) it took
// that fighter's z when it was made and keeps it (Ikemen's z-axis; Explod.setPos / update), following it only while
// bound (bindtime). Sparks and thrown effects therefore stay where they were made instead of riding along on the
// owner's current depth. Explods placed at the screen edges (postype front/back/left/right, none) carry no depth of
// their own: their owner's.
func ovCueExplod(e *Explod) {
	var v ovCueVals
	v.get() // (ovCueMakerGone: its cue keeps the depth of what was cued before it)
	ovCueExplodTo(&v, e)
	v.set()
}

// ovCueExplodTo: ovCueExplod's values into v. Nothing global is written. Every field of v is set unless
// ovCueMakerGone(e), which leaves v.Z
func ovCueExplodTo(v *ovCueVals, e *Explod) {
	v.RootZ = ovNoDepth // (ovCueCharTo's, when there is a fighter)
	if p := e.parent(); p != nil {
		ovCueCharTo(v, p, false)
	} else if len(sys.chars[e.playerno]) > 0 {
		if r := e.root(); r != nil {
			ovCueCharTo(v, r, false)
		}
	} else {
		v.Z = ovIkUnits(e.interPos[2], e.localscl)
	}
	if ovOwnDepthOn && e.space != Space_screen && (e.postype == PT_P1 || e.postype == PT_P2) {
		v.Z = ovIkUnits(e.interPos[2], e.localscl)
	}
	v.X = ovIkUnits(e.interPos[0]*e.localscl, 1)
	v.Y = ovIkUnits(e.interPos[1]*e.localscl, 1)
	v.AnchorY = v.Y
	v.Root, v.Kind, v.AnimNo, v.ExplodOf = ovRootOf(e.playerno), 2, e.animNo, e
}

// ovCueMakerGone: e has a maker slot whose root is missing - its cue keeps the depth of what was cued before it
func ovCueMakerGone(e *Explod) bool {
	return e.parent() == nil && len(sys.chars[e.playerno]) > 0 && e.root() == nil
}

// A projectile's card stands at its own depth (it was made at its owner's z plus its offset and hits only what its
// own z overlaps: Projectile.update / zAxisOverlap), not at its owner's current depth
func ovCueProjectile(p *Projectile) {
	ovCueRootZ = ovNoDepth // (ovCueCharFor's, when there is a fighter)
	if o := p.owner(); o != nil {
		ovCueCharFor(o, false)
	} else if len(sys.chars[p.playerno]) > 0 {
		ovCueCharFor(p.root(), false)
	}
	if ovOwnDepthOn {
		ovCueZ = ovIkUnits(p.interPos[2], p.localscl)
	}
	ovCueX = ovIkUnits(p.interPos[0], p.localscl)
	ovCueY = ovIkUnits(p.interPos[1], p.localscl)
	ovCueAnchorY = ovCueY
	ovCueRoot, ovCueKind, ovCueAnimNo = ovRootOf(p.playerno), 3, p.animNo
}

// ---------------------------------------------------------------------------------------------------------------------
// The huge-sprite option (init config "huge_cut": true; off by default, so everything a pack draws is drawn).
// Screen-filling flashes, backdrops and towering bolts made for a 2D screen stand in a 3D scene as giant cards. With
// the option on, an EFFECT sprite (helper, explod, projectile) drawn >= ovHugeTall of its root fighter's stand heights
// tall, or >= ovHugeBulk of them on both sides, is not drawn; nor is a root's own sprite of a screen's size
// (>= ovScreenW x ovScreenH root pixels: a backdrop). A slim beam (narrower than ovHugeSlim stand heights) may stand
// up to ovHugeSlimTall. Sizes are the drawn size in the root's own pixels. Each distinct sprite cut is logged once and
// reported to the host as a "huge_cut" event.

const (
	ovHugeTall     = 3.0
	ovHugeSlim     = 1.5 // (a beam: narrower than this many stand heights...)
	ovHugeSlimTall = 4.0 // (...may be this tall)
	ovHugeBulk     = 2.4
	ovScreenW      = 320.0
	ovScreenH      = 200.0
	ovRefHeight    = 130.0 // a fighter with no stance sprite
	ovMinStandH    = 60.0  // the least stand height a fighter is taken to have
	ovHugeLogKeep  = 400   // distinct cut sprites logged (overlay.log), then only counted
)

var ovHugeOn bool

var ovHugeStand = map[string]float32{} // root def (+ size scale) -> stand height (root px); host thread only
// ovHugeLogged: the cut sprites already logged - root, kind, action, sprite
type ovHugeKey struct {
	root         uintptr
	kind         int8
	anim         int32
	group, numbr uint16
}

var ovHugeLogged = map[ovHugeKey]bool{}
var ovHugeCuts int64 // sprite draws skipped

// ovStandHeight's answer per root fighter, valid while its def and size.yscale stay (a reused slot gets a new def).
// Kept on the root itself, so a despawned root is not held alive by a cache.
type ovStandEntry struct {
	def string
	ys  uint32 // (float bits)
	h   float32
}

// The root's stand height in its own pixels: its first stance frame (action 0, element 1), cached per def
func ovStandHeight(root *Char) float32 {
	gi := root.gi()
	ys := math.Float32bits(root.size.yscale)
	if e := &root.ovStand; e.def != "" && e.ys == ys && e.def == gi.def {
		return e.h
	}
	h := ovStandHeightOf(root, gi)
	root.ovStand = ovStandEntry{def: gi.def, ys: ys, h: h}
	return h
}

func ovStandHeightOf(root *Char, gi *CharGlobalInfo) float32 {
	key := fmt.Sprintf("%s|%g", gi.def, root.size.yscale)
	if h, ok := ovHugeStand[key]; ok {
		return h
	}
	h := float32(ovRefHeight)
	if a := gi.animTable.anims[0]; a != nil && len(a.frames) > 0 && gi.sff != nil {
		f := a.frames[0]
		if f.Group >= 0 && f.Number >= 0 {
			if spr := gi.sff.GetSprite(uint16(f.Group), uint16(f.Number)); spr != nil && spr.Size[1] > 0 {
				ys := root.size.yscale
				if ys == 0 {
					ys = 1
				}
				sh := float32(spr.Size[1])
				if box, ok := spr.ovContent(); ok && box[3] > box[1] {
					sh = float32(box[3] - box[1]) // (a padded stance canvas: its drawn pixels)
				}
				h = sh * Abs(ys) * Abs(f.Yscale)
				if h < ovMinStandH {
					h = ovMinStandH
				}
			}
		}
	}
	ovHugeStand[key] = h
	return h
}

// Whether the overlay skips this sprite (DrawList.draw, overlay rendering with the huge-sprite option on)
func ovHugeCut(s *SpriteData) bool {
	a, root := s.anim, s.ovRoot
	if a == nil || a.spr == nil || root == nil || root.localcoord <= 0 {
		return false
	}
	hs, vs := float32(1), float32(1)
	if a.drawidx >= 0 && int(a.drawidx) < len(a.frames) {
		hs, vs = float32(a.frames[a.drawidx].Hscale), float32(a.frames[a.drawidx].Vscale)
	}
	// the sprite's drawn pixels, not its canvas (some frames are padded to screen-sized canvases)
	cw, ch := float32(a.spr.Size[0]), float32(a.spr.Size[1])
	if box, ok := a.spr.ovContent(); ok {
		if box[2] <= box[0] || box[3] <= box[1] {
			return false // (nothing drawn)
		}
		cw, ch = float32(box[2]-box[0]), float32(box[3]-box[1])
	}
	// An explod is measured at the largest scale it has had so far, so one that shrinks stays cut as it shrinks
	sx, sy := Abs(s.scl[0]), Abs(s.scl[1])
	if e := s.ovExplod; e != nil {
		e.ovSclMax[0] = Max(e.ovSclMax[0], sx)
		e.ovSclMax[1] = Max(e.ovSclMax[1], sy)
		sx, sy = e.ovSclMax[0], e.ovSclMax[1]
	}
	base := 320 / root.localcoord // game units per root px (Char/Explod/Projectile cueDraw's basescale)
	w := cw * sx * Abs(hs*a.scale_x) / base
	h := ch * sy * Abs(vs*a.scale_y) / base
	rule, ref := "", float32(0)
	if s.ovKind == 0 {
		if w >= ovScreenW && h >= ovScreenH {
			rule = "screen"
		}
	} else {
		ref = ovStandHeight(root)
		if h >= ovHugeTall*ref && !(w < ovHugeSlim*ref && h < ovHugeSlimTall*ref) {
			rule = "tall"
		} else if w >= ovHugeBulk*ref && h >= ovHugeBulk*ref {
			rule = "bulk"
		} else if w >= ovScreenW && w >= 2*h &&
			float32(a.spr.Size[0])*sx*Abs(hs*a.scale_x)/base >= ovScreenW &&
			float32(a.spr.Size[1])*sy*Abs(vs*a.scale_y)/base >= ovScreenH {
			// (a picture of a whole screen, drawn a screen wide and at least twice as wide as tall: a screen-wide sweep
			// whose drawn pixels are a long low strip that the two rules above let through)
			rule = "wide"
		}
	}
	if rule == "" {
		return false
	}
	ovHugeCuts++
	key := ovHugeKey{uintptr(unsafe.Pointer(root)), s.ovKind, s.ovAnimNo, a.spr.Group, a.spr.Number}
	if !ovHugeLogged[key] && len(ovHugeLogged) < ovHugeLogKeep {
		ovHugeLogged[key] = true
		kind := [...]string{"body", "helper", "explod", "projectile"}[s.ovKind&3]
		LogMessage("[overlay] huge cut (%s): %s (%s) %s anim %d sprite %d,%d %dx%d (drawn pixels %.0fx%.0f) drawn %.0fx%.0f px, stand %.0f px, trans %d",
			rule, root.gi().displayname, filepath.Base(root.gi().def), kind, s.ovAnimNo, a.spr.Group, a.spr.Number,
			a.spr.Size[0], a.spr.Size[1], cw, ch, w, h, ref, a.transType)
		ovPushEvent(map[string]interface{}{"e": "huge_cut", "h": root.playerNo, "kind": kind, "anim": s.ovAnimNo,
			"spr": []int{int(a.spr.Group), int(a.spr.Number)}, "w": int(w), "hgt": int(h), "rule": rule})
	}
	return true
}

// ---------------------------------------------------------------------------------------------------------------------
// Card projection. Ikemen computes every sprite's quad in its own 2D screen space as always; RenderSprite then uses
// projection = VP * B(z) instead of the 2D ortho matrix, where B maps Ikemen's screen pixels (GL convention, y up)
// onto a camera-facing plane standing at the sprite owner's depth z, 1 ik unit = params16[0] world units.
//
// What the host passes in ikemen_render's params16:
//   [0] world units (metres) per ik unit (0: ovDefaultUnit)
//   [1] world x of ik x = 0            [2] world z of ik z (depth) = 0
//   [3], [4] the cards' right vector in the world's x/z plane (0, 0: world +x)
//   [5] world y of the ground line (the height of the host's floor)
//   [6] 1: the host's depth buffer is standard (nearer = smaller, LEQUAL); else reverse-Z (nearer = larger, GEQUAL)
//   [7] depth bias: world units toward the camera a card is moved for the depth test only
//   [8..15] unused

const ovDefaultUnit = 0.01

type ovCardState struct {
	vp                   mgl.Mat4 // host view-projection (column-major), Y flip folded in when FLIP_Y
	m                    float32  // world units per ik unit
	worldX0              float32  // world x of ik_x = 0
	worldZ0              float32  // world z of ik_z = 0
	worldY0              float32  // world y of the ground line
	rx, rz               float32  // card right vector (camera basis x, y removed, normalized)
	ux, uy, uz           float32  // card up vector: the camera's up (cards face the camera: setFacing), or world up
	persp                bool     // the host's camera is a perspective one (setFacing)
	ox, oy               float32  // GL pixel position of ik (0, 0)
	kx, ky               float32  // GL pixels per ik unit
	kxPrecise, kyPrecise float64  // kx, ky before rounding (the basis is composed in double precision, rounded once)
	z, ax                float32  // current sprite's owner depth and anchor x (ik units)
	ay                   float32  // its anchor's height (ik units, + down): moves along world up, not the card's
	basis                mgl.Mat4 // the current card: GL pixels -> world (setSprite)
	depthBias            float32  // world units toward the camera for the depth test only (params16[7])
	cur                  mgl.Mat4 // VP * B(z) for the current sprite
	curValid             bool
	screen               bool       // the current sprite is a screen-space one: cur is the flat 2D projection (setScreen)
	fy, zFront           float32    // ...its y flip, and the clip z that passes the host's depth test
	scrW, scrH           float32    // ...Ikemen's screen in GL pixels
	smooth               bool       // texel edges antialiased this frame (sprite.frag.glsl ovSmooth)
	floor                [4]float32 // the floor rule's window-space depth plane A, B, C and its sign (0: off)
	floorOn              bool       // the current sprite takes the floor rule
	floorSign            float32    // (setFloor's sign: 0 no floor rule this frame)
	upright              [4]float32 // the late pass: the window-space depth plane the current sprite leaves (host_ov_depth.go)
	uprightAt            [4]float32 // ...and the one it is tested against the scene with (at its anchor, unmoved)
	uprightKey           float32    // (that plane's distance from the card origin; uprightSet: worked out this frame)
	uprightSet           bool
	viewW, viewH         int32 // (setFloor's target size and depth range, for those planes)
	depthRange           [2]float32
}

// setCamera: the frame's host camera - the view-projection and params16 of ikemen_render (see the table above)
func (c *ovCardState) setCamera(vp, params [16]float32, flags int) {
	c.vp = ovHostVP(vp, params, flags)
	c.m = params[0]
	if c.m == 0 {
		c.m = ovDefaultUnit
	}
	c.worldX0, c.worldZ0, c.worldY0 = params[1], params[2], params[5]
	c.depthBias = params[7]
	c.rx, c.rz = params[3], params[4]
	if n := float32(math.Hypot(float64(c.rx), float64(c.rz))); n > 1e-6 {
		c.rx, c.rz = c.rx/n, c.rz/n
	} else {
		c.rx, c.rz = 1, 0
	}
	c.setFacing()
	c.fy, c.zFront = 1, 0
	if flags&ovFlagFlipY != 0 {
		c.fy = -1
	}
	if flags&ovFlagDepthTest != 0 {
		c.zFront = 0.999999 // (reverse-Z: the nearest depth is the largest)
		if params[6] == 1 {
			c.zFront = -0.999999
		}
	}
	c.smooth = ovSmoothOn && c.persp && !ovCfg.ParityMode
	c.curValid, c.screen = false, false
	c.z, c.ax, c.ay = 0, 0, 0
	c.floorOn = false
}

// ovFloorSignOf: the floor rule's sign for a frame drawn with these flags - 0 off (no depth test against the host's
// scene, the parity profile, or config "floor_rule": false), +1 a reverse-Z host, -1 a standard depth buffer
func ovFloorSignOf(flags int, params [16]float32) float32 {
	if flags&ovFlagDepthTest == 0 || ovCfg.ParityMode || !ovFloorRuleOn {
		return 0
	}
	if params[6] == 1 {
		return -1
	}
	return 1
}

// ---------------------------------------------------------------------------------------------------------------------
// The floor rule. A 2D fighting game draws whatever lies below a sprite's ground line ON the floor in front of it:
// feet a few pixels low, the legs of a big creature, effects lying on the ground. On a card standing on the host's
// floor those pixels are under the floor, and the depth test would cut them off along a straight line. Rule: a sprite
// pixel's depth for the test is the NEARER of its card's depth and the floor's own depth at that pixel, the floor
// lifted by ovFloorLift - so a pixel above the ground line is tested where its card stands, a pixel below it as if it
// lay on the floor where its view ray meets the floor. 3D props standing between that floor point and the camera
// still hide it; the floor itself never does. Position, size and blending stay exact (only the depth the test uses
// changes: sprite.frag.glsl, gl_FragDepth). The host's floor is taken to be the horizontal plane at params16[5].
// Off without DEPTH_TEST, in the parity profile and with init config "floor_rule": false.

// ovFloorLift: world units above the host's floor, so the floor's own depth never wins the depth test against what
// lies on it at the precision of the host's depth buffer
const ovFloorLift = 0.15

var ovFloorRuleOn = true

// ovOwnDepthOn: explods and projectiles stand at their own depth (ovCueExplod); env IKEMEN_OWN_DEPTH=0 puts them on
// their owner's current depth
var ovOwnDepthOn = os.Getenv("IKEMEN_OWN_DEPTH") != "0"

// ovNoDepthDiag: (diagnostic, env IKEMEN_OV_NODEPTH=1) sprites drawn with no depth test against the host's 3D scene
var ovNoDepthDiag = os.Getenv("IKEMEN_OV_NODEPTH") == "1"

// Window-space depth plane of the lifted floor: depth = A*x + B*y + C at window pixel (x, y), from three floor points
// through the frame's VP (flips included: the same transform the sprite vertices get). Any plane's image under a
// projective map keeps NDC depth affine in NDC x/y (also past the horizon, where it keeps growing farther than far,
// so the nearer-of rule picks the card there). sign +1: reverse-Z host (nearer = larger, max); -1: standard depth
// host (min). depthRange: glDepthRange (window z = n + (f-n)*(z_ndc+1)/2).
func (c *ovCardState) setFloor(w, h int32, depthRange [2]float32, sign float32) {
	c.floor = [4]float32{}
	c.viewW, c.viewH, c.depthRange = w, h, depthRange
	c.upright, c.uprightAt, c.uprightSet = [4]float32{}, [4]float32{}, false // (the late pass's planes, this frame's camera)
	c.floorSign = sign
	if sign == 0 || w <= 0 || h <= 0 {
		return
	}
	c.floor = c.floorPlaneAt(c.worldY0 + ovFloorLift)
}

// ovVP64: point (x, y, z) through the projection vp in double precision. A plane's depth slope is the difference of
// window depths a world unit apart that agree to 4-5 digits: through float32 the slope came out in steps up to a fifth
// of itself, changing from frame to frame as the camera moved, and rows below a sprite's ground line flickered.
func ovVP64(vp *mgl.Mat4, x, y, z float64) [4]float64 {
	var q [4]float64
	for row := 0; row < 4; row++ {
		q[row] = float64(vp[row])*x + float64(vp[4+row])*y + float64(vp[8+row])*z + float64(vp[12+row])
	}
	return q
}

// floorPlaneAt: setFloor's window-space depth plane for the horizontal plane at world height y; zero when there is
// none this frame
func (c *ovCardState) floorPlaneAt(y float32) [4]float32 {
	w, h, depthRange, sign := c.viewW, c.viewH, c.depthRange, c.floorSign
	var win [3][3]float64
	base, ok := ovFloorBase(c.vp, y)
	if !ok {
		return [4]float32{} // (no floor point in front of the camera at all: no plane this frame, the card depth alone)
	}
	// (the steps from it scaled with its distance: a plane through any three floor points is the same plane, and unit
	// steps at a point near the horizon come out a sliver of a triangle on screen, the plane then dropped as edge-on)
	step := 1.0
	if q0 := ovVP64(&c.vp, float64(base[0]), float64(y), float64(base[1])); q0[3] > 4 {
		step = q0[3] / 4
	}
	for i, d := range [3][2]float64{{0, 0}, {step, 0}, {0, step}} { // (from a floor point the camera looks at)
		q := ovVP64(&c.vp, float64(base[0])+d[0], float64(y), float64(base[1])+d[1])
		if q[3] < 1e-4 {
			return [4]float32{} // (a floor point behind the camera: no plane this frame, the card depth alone)
		}
		iw := 1 / q[3]
		win[i] = [3]float64{
			(q[0]*iw*0.5 + 0.5) * float64(w),
			(q[1]*iw*0.5 + 0.5) * float64(h),
			float64(depthRange[0]) + float64(depthRange[1]-depthRange[0])*(q[2]*iw*0.5+0.5),
		}
	}
	d1 := [3]float64{win[1][0] - win[0][0], win[1][1] - win[0][1], win[1][2] - win[0][2]}
	d2 := [3]float64{win[2][0] - win[0][0], win[2][1] - win[0][1], win[2][2] - win[0][2]}
	nx := d1[1]*d2[2] - d1[2]*d2[1]
	ny := d1[2]*d2[0] - d1[0]*d2[2]
	nz := d1[0]*d2[1] - d1[1]*d2[0]
	if math.Abs(nz) < 1e-3 { // (the floor seen edge-on: no usable plane)
		return [4]float32{}
	}
	a, b := -nx/nz, -ny/nz
	return [4]float32{float32(a), float32(b), float32(win[0][2] - a*win[0][0] - b*win[0][1]), sign}
}

// ovFloorBase: a point (world x, z) of the horizontal plane at height y the camera sees - where a view ray through
// the screen meets it, tried from the lower middle of the screen outwards. The plane through any three floor points
// is the same plane; these are never behind the camera.
func ovFloorBase(vp mgl.Mat4, lift float32) ([2]float32, bool) {
	var m mgl64.Mat4
	for i := range vp {
		m[i] = float64(vp[i])
	}
	inv := m.Inv()
	for _, xy := range [][2]float64{{0, -0.5}, {0, 0.5}, {0, 0}, {0, -0.85}, {0, 0.85}, {-0.6, -0.5}, {0.6, -0.5},
		{-0.6, 0.5}, {0.6, 0.5}} {
		a := inv.Mul4x1(mgl64.Vec4{xy[0], xy[1], -0.5, 1})
		b := inv.Mul4x1(mgl64.Vec4{xy[0], xy[1], 0.5, 1})
		if math.Abs(a[3]) < 1e-12 || math.Abs(b[3]) < 1e-12 {
			continue
		}
		pa := mgl64.Vec3{a[0] / a[3], a[1] / a[3], a[2] / a[3]}
		pb := mgl64.Vec3{b[0] / b[3], b[1] / b[3], b[2] / b[3]}
		dy := pb[1] - pa[1]
		if math.Abs(dy) < 1e-9 {
			continue
		}
		t := (float64(lift) - pa[1]) / dy
		p := pa.Add(pb.Sub(pa).Mul(t))
		ok := true
		for _, d := range [3][2]float64{{0, 0}, {1, 0}, {0, 1}} {
			if q := m.Mul4x1(mgl64.Vec4{p[0] + d[0], float64(lift), p[2] + d[1], 1}); q[3] < 1e-3 {
				ok = false
				break
			}
		}
		if ok {
			return [2]float32{float32(p[0]), float32(p[2])}, true
		}
	}
	return [2]float32{}, false
}

// The floor rule's uniform for the current sprite (sprite.frag.glsl ovFloor): w = 0 leaves the card's depth alone
func ovFloorUniform() [4]float32 {
	if !ovCard.floorOn {
		return [4]float32{}
	}
	return ovCard.floor
}

var ovCard ovCardState
var ovRendering bool

// setSprite: the card of the sprites that follow - their owner's depth z, anchor x and anchor height ay (ik units)
func (c *ovCardState) setSprite(z, ax, ay float32) {
	if ovCfg.ParityMode {
		ay = 0 // (the parity profile: the card's plain geometry, with its exact cache and arithmetic)
	}
	if c.curValid && !c.screen && z == c.z && ay == c.ay && (ax == c.ax || (c.rx == 1 && c.rz == 0)) {
		return
	}
	c.screen = false
	c.z, c.ax = z, ax
	c.ay = ay
	m := c.m
	// P = A + R*(u-ax)*m + Up*v*m, A = (X0 + ax*m, Y0, Z0 + z*m), u = (gx-ox)/kx, v = (gy-oy)/ky
	sx, sy := m/c.kx, m/c.ky
	// Translation: X0 + ax*m - rx*m*(ox/kx + ax) with the anchor term cancelled algebraically before float32
	// arithmetic: adding and subtracting ax*m lost bits, so a camera-facing X card (rx = 1, where the cache above
	// ignores ax) moved by the rounding of whichever owner's anchor came first at that depth - sub-pixel jitter that
	// moved texel-edge samples
	b := mgl.Mat4{
		c.rx * sx, 0, c.rz * sx, 0, // column 0: d/dgx
		0, sy, 0, 0, // column 1: d/dgy
		0, 0, 0, 0, // column 2: quads are flat (z = 0)
		c.worldX0 - c.rx*sx*c.ox + (1-c.rx)*ax*m, -sy * c.oy, c.worldZ0 + z*m - c.rz*(sx*c.ox+ax*m), 1,
	}
	if c.uy != 1 { // (a card facing the camera: its up is the camera's, pivoting at the ground point - setFacing)
		b[4], b[5], b[6] = c.ux*sy, c.uy*sy, c.uz*sy
		b[12] -= c.ux * sy * c.oy
		b[13] = -c.uy * sy * c.oy
		b[14] -= c.uz * sy * c.oy
	}
	if ay != 0 {
		// The 2D quad already contains -y along card up. Replace that anchor motion with -y along world up.
		b[12] += m * ay * c.ux
		b[13] += m * ay * (c.uy - 1)
		b[14] += m * ay * c.uz
	}
	if c.worldY0 != 0 {
		b[13] += c.worldY0
	}
	c.cur = c.vp.Mul4(b)
	c.basis = b
	// Basis columns: the camera/local/framebuffer factors kept in double precision and the composed basis rounded
	// once, instead of rounding each product (small effects straddle a texel boundary even when the card moves by
	// less than 1/10000 pixel)
	sx64, sy64 := float64(m)/c.kxPrecise, float64(m)/c.kyPrecise
	for row := 0; row < 4; row++ {
		c.cur[row] = float32((float64(c.vp[row])*float64(c.rx) + float64(c.vp[8+row])*float64(c.rz)) * sx64)
		if c.uy != 1 {
			c.cur[4+row] = float32((float64(c.vp[row])*float64(c.ux) + float64(c.vp[4+row])*float64(c.uy) +
				float64(c.vp[8+row])*float64(c.uz)) * sy64)
		} else {
			c.cur[4+row] = float32(float64(c.vp[4+row]) * sy64)
		}
	}
	// Depth bias (params16[7], world units toward the camera): the depth test uses the card moved forward along its
	// normal, so the floor does not cut off feet and ground effects drawn a few pixels below the ground line.
	// Position and size stay exact; only the clip z row changes. Any perspective projection has z_clip = a*w_clip + b,
	// so the row becomes a*w + b*w0/w1 (w0, w1: clip w of a mid-body point on the card and on the moved card), which
	// gives the moved card's exact depth at that point and differs little over a fighter's height.
	if c.depthBias != 0 {
		p0 := mgl.Vec4{c.worldX0 + ax*m, c.worldY0 + 0.9, c.worldZ0 + z*m, 1}
		if !ovCfg.ParityMode {
			p0 = mgl.Vec4{c.worldX0 + ax*m + 0.9*c.ux, c.worldY0 - ay*m + 0.9*c.uy, c.worldZ0 + z*m + 0.9*c.uz, 1}
		}
		p1 := mgl.Vec4{p0[0] - c.rz*c.depthBias, p0[1], p0[2] + c.rx*c.depthBias, 1}
		q0, q1 := c.vp.Mul4x1(p0), c.vp.Mul4x1(p1)
		if dw := q1[3] - q0[3]; (dw > 1e-6 || dw < -1e-6) && q1[3] != 0 {
			a := (q1[2] - q0[2]) / dw
			bb := q0[2] - a*q0[3]
			for col := 0; col < 4; col++ {
				c.cur[col*4+2] = a * c.cur[col*4+3]
			}
			c.cur[14] += bb * q0[3] / q1[3]
		} else { // orthographic host camera: w is constant, the moved card's z row is exact
			nb := b
			nb[12] += -c.rz * c.depthBias
			nb[14] += c.rx * c.depthBias
			biased := c.vp.Mul4(nb)
			for col := 0; col < 4; col++ {
				c.cur[col*4+2] = biased[col*4+2]
			}
		}
	}
	c.curValid = true
}

// setScreen: the projection of a screen-space sprite (an explod or text placed on Ikemen's 2D screen, not in the
// stage): Ikemen's own flat 2D projection, its screen stretched over the host's frame as stock Ikemen stretches it
// over its window, in front of everything in the host's scene. No floor rule, no depth left for the host.
func (c *ovCardState) setScreen() {
	if c.curValid && c.screen {
		return
	}
	c.screen, c.curValid = true, true
	if c.scrW <= 0 || c.scrH <= 0 {
		c.scrW, c.scrH = float32(sys.scrrect[2]), float32(sys.scrrect[3])
	}
	c.cur = mgl.Mat4{
		2 / c.scrW, 0, 0, 0,
		0, 2 / c.scrH * c.fy, 0, 0,
		0, 0, 0, 0,
		-1, -c.fy, c.zFront, 1,
	}
	c.upright, c.uprightAt, c.uprightSet = [4]float32{}, [4]float32{}, false
}

// setFacing: the frame's card up vector. Cards face the camera - parallel to the screen, pivoting at the sprite's
// ground point - so every sprite is its own picture scaled by its distance, never distorted, wherever it is on the
// screen; an upright card seen from above is squashed and swings what lies below its ground line into the floor as
// the camera moves. A card parallel to the screen also lies at one view depth: it is depth-tested at its ground
// point, as 2.5D games test a sprite at its feet. Up = world up made perpendicular to the camera's view direction
// (the view-projection's w row: w is the view depth). The parity profile, an orthographic host and env
// IKEMEN_CARD=upright keep the upright card (world up).
func (c *ovCardState) setFacing() {
	c.ux, c.uy, c.uz = 0, 1, 0
	fx, fy, fz := float64(c.vp[3]), float64(c.vp[7]), float64(c.vp[11])
	n := math.Sqrt(fx*fx + fy*fy + fz*fz)
	c.persp = n >= 1e-9 // (orthographic: w carries no view direction)
	if ovCfg.ParityMode || !ovFacingOn || !c.persp {
		return
	}
	fx, fy, fz = fx/n, fy/n, fz/n
	ux, uy, uz := -fx*fy, 1-fy*fy, -fz*fy
	if l := math.Sqrt(ux*ux + uy*uy + uz*uz); l > 1e-6 && uy > 0 {
		c.ux, c.uy, c.uz = float32(ux/l), float32(uy/l), float32(uz/l)
	}
}

var ovFacingOn = os.Getenv("IKEMEN_CARD") != "upright"

// ovScreenSpritesOn: (init config "screen_sprites", default true) screen-space sprites are drawn, flat over the frame
// (setScreen); false: they are skipped, for a host that draws its whole 2D layer itself
var ovScreenSpritesOn = true

// ovSmoothOn: (init config "smooth", default true) on a perspective host camera a magnified indexed sprite's texels
// are drawn whole with their edges antialiased - a pixel's width of blend with the neighbour (sprite.frag.glsl
// ovTexelAA) - so they slide as the camera moves instead of jumping whole pixels. The colours inside each texel are
// untouched. Never in the parity profile or on an orthographic (flat 2D) host camera: those stay nearest-texel.
var ovSmoothOn = true

// ovSmoothNow: how the sprite about to be drawn is sampled (sprite.frag.glsl ovSmooth): 1 texel edges antialiased,
// 0 the nearest texel
func ovSmoothNow() int32 {
	if ovCard.smooth && !ovCard.screen {
		return 1
	}
	return 0
}

// Render-thread work of one overlay frame (Sprite.ovEnsureTex): decodes (fight-screen SFFs only, see ovLazySff) and
// texture uploads at a sprite's first draw; a slow frame is logged with them, totals at shutdown
var ovFrameDecodes, ovFrameUploads int
var ovFrameDecodeUs, ovFrameUploadBytes int64
var ovTotalDecodes, ovTotalUploads, ovSlowRenders int64
var ovTotalDecodeUs, ovTotalUploadBytes int64

func ovRenderStats(t0 time.Time, sprites int) {
	d := time.Since(t0)
	ovRenderStatsAdd(d.Microseconds()) // (host_ov_render.go: us/frame, sprites/frame, us/sprite every 600 frames)
	ovTotalDecodes += int64(ovFrameDecodes)
	ovTotalDecodeUs += ovFrameDecodeUs
	ovTotalUploads += int64(ovFrameUploads)
	ovTotalUploadBytes += ovFrameUploadBytes
	if d >= 5*time.Millisecond {
		ovSlowRenders++
		if ovSlowRenders <= 60 {
			LogMessage("[overlay] slow render %.1f ms at tick %d: %d sprites cued, %d decodes (%.1f ms), %d uploads (%d KB)",
				float64(d.Microseconds())/1000, atomic.LoadUint64(&ovTicks), sprites, ovFrameDecodes, float64(ovFrameDecodeUs)/1000,
				ovFrameUploads, ovFrameUploadBytes>>10)
		}
	}
}

// The projection RenderSprite uses for the current sprite in overlay rendering
func ovSpriteProjection() mgl.Mat4 {
	if !ovCard.curValid {
		if ovCard.screen {
			ovCard.setScreen()
		} else {
			ovCard.setSprite(ovCard.z, ovCard.ax, ovCard.ay)
		}
	}
	return ovCard.cur
}

// ---------------------------------------------------------------------------------------------------------------------
// The overlay frame: runs on the host's thread (GL context current) while the Ikemen thread is parked.

func (s *System) ovRenderFrame(fbo uint32, w, h int32, flags int, vp, params [16]float32) int {
	// A replayed clip is always drawn by the frozen-snapshot renderer (pipeline on or off): its recorded draw list is
	// captured through the same draw path into the replay's own snapshot (ovRenderReplay)
	if ovReplayIsPlaying() {
		return ovRenderReplay(s, fbo, w, h, flags, vp, params)
	}
	if ovCfg.Pipeline {
		return ovRenderDrawSnapshot(fbo, w, h, flags, vp, params)
	}
	return s.ovRenderLiveFrame(fbo, w, h, flags, vp, params)
}

// The direct renderer (pipeline off): Ikemen's draw list drawn on the host's thread while the Ikemen thread is parked.
func (s *System) ovRenderLiveFrame(fbo uint32, w, h int32, flags int, vp, params [16]float32) int {
	r, ok := gfx.(*Renderer_GL33)
	if !ok {
		ovSetError("overlay rendering needs the OpenGL 3.3 renderer")
		return -1
	}
	r.ovBeginFrame(fbo, w, h, flags, params[6] == 1)
	defer r.ovEndFrame()

	if s.stage == nil {
		return 0
	}

	// (No aspect swap as in renderFrame: KeepAspect is off in overlay mode, the fight aspect is what the sprites were
	// cued in, and swapping sys.gameWidth here would race with the loader goroutine reading it)

	x, y, scl := s.cam.Pos[0], s.cam.Pos[1], s.cam.Scale/s.cam.BaseScale()
	cs := scl * s.cam.BaseScale()
	var shake [2]float32
	if !ovCfg.SpriteShake || ovCfg.ParityMode {
		shake = s.envShake.getOffset() // cancels Ikemen's screen shake on the sprites (the host shakes its camera)
		// The recorded flat VP includes the native shake in its origin. Use
		// that same origin in B so the composed projection retains it once.
	}
	gu := s.gameWidth / ovCfg.IkLocalcoord

	c := &ovCard
	// The host passes its plain P * V; ovHostVP applies the flips its own 3D pass uses (y under FLIP_Y, clip z for a
	// reverse-Z depth buffer under DEPTH_TEST unless params16[6] = 1)
	c.setCamera(vp, params, flags)
	c.scrW, c.scrH = float32(s.scrrect[2]), float32(s.scrrect[3])
	c.ox = s.widthScale * (s.gameWidth/2 + s.cam.Offset[0] - shake[0] - cs*x)
	c.oy = float32(s.scrrect[3]) - s.heightScale*(s.cam.GroundLevel()+s.cam.Offset[1]-shake[1]-y)
	c.kx = s.widthScale * cs * gu
	c.ky = s.heightScale * cs * gu
	// (the same factors unrounded, for the basis composed in double precision - setSprite)
	c.kxPrecise = float64(s.widthScale) * float64(cs) * float64(gu)
	c.kyPrecise = float64(s.heightScale) * float64(cs) * float64(gu)
	if c.kx == 0 || c.ky == 0 {
		return 0
	}
	// The floor rule (see setFloor): only against a host's depth, never in the parity profile
	floorSign := ovFloorSignOf(flags, params)
	c.setFloor(w, h, r.ovDepthRange, floorSign)

	// Dev A/B (env IKEMEN_RENDER_AB, host_ov_render.go): this frame drawn by the generic and the fast sprite path into
	// copies of the host's target and compared; then the host's frame is begun again exactly as above
	if ovABDue() {
		s.ovRenderAB(r, fbo, w, h, flags, params[6] == 1, x, y, scl, floorSign)
		r.ovBeginFrame(fbo, w, h, flags, params[6] == 1)
		c.curValid, c.screen = false, false
		c.z, c.ax, c.ay = 0, 0, 0
		c.setFloor(w, h, r.ovDepthRange, floorSign)
	}

	ovPprofCheck()
	ovAltFrame() // (dev: IKEMEN_RENDER_ALT)
	t0 := time.Now()
	ovFrameDecodes, ovFrameDecodeUs, ovFrameUploads, ovFrameUploadBytes = 0, 0, 0, 0
	ovFrameSprites, ovFrameDraws, ovFrameSame, ovFrameSameButProj, ovFrameGLDraws = 0, 0, 0, 0, 0
	defer ovRenderStats(t0, len(s.spriteList))

	ovRendering = true
	s.draw(x, y, scl)
	// DEBUG_CLSN: Ikemen's own collision boxes (cued from the next tick on; drawn on the last sprite's card)
	s.clsnDisplay = flags&ovFlagDebugClsn != 0
	if s.clsnDisplay {
		s.clsnSpr.ovEnsureTex()
		s.drawTop()
	}
	ovGLDirect() // (the fast path's queued GL calls run now, inside the frame's timing - host_ov_render.go)
	ovRendering = false
	s.luaDiscardDrawQueue()
	ovReplayWarm() // (recorded clips' textures readied a few at a time, after the queue ran: uploads go direct)
	return 1
}
