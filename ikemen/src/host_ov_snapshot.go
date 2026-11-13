package main

// The pipeline's draw snapshots, also the replay reel's draw path. Capture runs on the simulation thread after
// cueDraw (pipeline), or on the host while the world holds (a replayed clip), through Ikemen's existing draw math:
// System.draw -> DrawList.draw -> Animation.Draw -> RenderSprite, with RenderSprite/FillRect recording resolved
// RenderParams and ShaderPalFX instead of calling GL, and DrawList.draw recording each sprite owner's card step
// (depth, anchor, floor rule). Render consumes only those frozen ops - never Animation/Char/Explod/PalFX/Lua - with
// the same card projection, floor rule, fast sprite path and GL queue as the live renderer.
// Textures are strong references; GL creation/upload/delete remains in mainThreadTask on the host. Two pooled buffers
// with a short publication barrier; a reader pins its buffer until the draw has finished.

import (
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type ovDrawOp struct {
	rp     RenderParams
	z, x   float32 // owner step: the card's depth and anchor x (ik units)
	y      float32 // owner step: the anchor's height, never read from mutable sim state while rendering
	owner  bool    // an owner step (DrawList.draw -> ovSetDrawOwner): the card and floor rule change, nothing is drawn
	floor  bool    // owner step: the floor rule applies to the sprites that follow (ovCard.floorOn)
	screen bool    // owner step: the sprites that follow are screen-space ones (ovCard.setScreen)
	fill   bool
	debug  bool
	rect   [4]int32
	color  uint32
	alpha  [2]int32
	fx     ShaderPalFX
}

type ovDrawSnapshot struct {
	ops                              []ovDrawOp
	sort                             []int // capture-only scratch, reused across the four layer passes
	screen                           [4]int32
	aspect, shaderTime, shaderAspect float32
	card                             ovCardState
	state                            [MaxSimul * 2 * ovREC]float32
	stateN                           int
	tick                             uint64
	buildUs                          int64
	simulationUs                     int64
}

var ovSnapshots struct {
	sync.RWMutex
	buffers [3]ovDrawSnapshot
	pins    [3]atomic.Int32 // (IKEMEN_PUB_TRIPLE: draws pinning each buffer)
	latest  *ovDrawSnapshot
	next    int
}

// Three pooled buffers - the draw pins the latest for its whole frame (a count, its read lock held only to take it)
// and the capture writes one neither latest nor pinned, so publication never waits for the draw. Env
// IKEMEN_PUB_TRIPLE=0: two buffers, the draw's read lock held over its frame, publication waiting for it.
var ovPubTriple = os.Getenv("IKEMEN_PUB_TRIPLE") != "0"

// Only the capturing thread dereferences these. The atomic thread ID keeps render callbacks out of capture without
// sharing a mutable rendering flag.
var ovCaptureThread atomic.Uint64
var ovCapture *ovDrawSnapshot
var ovCaptureDebug bool
var ovSnapshotDebug atomic.Int32
var ovSimulationStarted int64  // QPC microseconds, simulation thread only
var ovConsoleMu sync.Mutex     // async CNS warnings and live VM warnings share the console
var ovSffBorrowMu sync.RWMutex // published SFF/SND pointers while other slots reset/load

func ovCapturing() bool {
	id := ovCaptureThread.Load()
	return id != 0 && id == ovCurrentThreadID()
}

func ovDrawActive() bool { return ovCapturing() || ovRendering }

// The owner of the sprites DrawList.draw draws next. Live rendering sets the card now; capture records the step, so
// the frozen renderer makes exactly the same setSprite calls in the same order (setSprite's cache is order-dependent).
// screen: the sprites are screen-space ones - Ikemen's flat 2D projection instead of a card (z, x, y, floor unused).
func ovSetDrawOwner(z, x, y float32, floor, screen bool) {
	if ovCapturing() {
		ovCapture.ops = append(ovCapture.ops, ovDrawOp{owner: true, z: z, x: x, y: y, floor: floor, screen: screen})
		return
	}
	ovCard.setOwner(z, x, y, floor, screen)
	ovFrameSprites++
}

// setOwner: the card (or, screen, the flat 2D projection) and the floor rule of the sprites that follow
func (c *ovCardState) setOwner(z, x, y float32, floor, screen bool) {
	if screen {
		c.setScreen()
		c.floorOn = false
		return
	}
	c.setSprite(z, x, y)
	c.floorOn = floor
}

func ovCaptureRenderParams(rp RenderParams) {
	fx := ShaderPalFX{mult: [3]float32{1, 1, 1}}
	if rp.pfx != nil {
		fx = rp.pfx.getFinalPalFx(rp.blendMode, rp.blendAlpha)
	}
	rp.pfx, rp.window = nil, nil // no mutable world pointers escape
	rp.ovFrame, rp.ovFx = ovCapture, fx
	ovCapture.ops = append(ovCapture.ops, ovDrawOp{rp: rp, debug: ovCaptureDebug})
}

// One frame's draw ops through System.draw on this thread (the simulation thread at a tick's end, or the host while
// the world holds for a replayed clip). debug: Ikemen's collision boxes when the host asked for them.
func ovCaptureFrame(s *System, f *ovDrawSnapshot, debug bool) {
	// Every op this capture records is written whole (append of a full ovDrawOp), so only the old ops past the new
	// count still hold texture references: released after the capture (they were all zeroed first, ~1 MB a tick)
	old := len(f.ops)
	defer func() {
		if n := len(f.ops); n < old {
			tail := f.ops[n:old]
			for i := range tail {
				tail[i] = ovDrawOp{}
			}
		}
	}()
	f.ops = f.ops[:0]
	f.screen, f.aspect = s.scrrect, s.getCurrentAspect()
	f.shaderAspect = f.aspect / s.getFightAspect()
	f.shaderTime = float32(s.frameCounter)
	if s.middleOfMatch() {
		f.shaderTime = float32(s.gameTime())
	}
	x, y, scl := s.cam.Pos[0], s.cam.Pos[1], s.cam.Scale/s.cam.BaseScale()
	cs := scl * s.cam.BaseScale()
	var shake [2]float32
	if !ovCfg.SpriteShake || ovCfg.ParityMode {
		shake = s.envShake.getOffset()
	}
	gu := s.gameWidth / ovCfg.IkLocalcoord
	f.card = ovCardState{
		ox: s.widthScale * (s.gameWidth/2 + s.cam.Offset[0] - shake[0] - cs*x),
		oy: float32(s.scrrect[3]) - s.heightScale*(s.cam.GroundLevel()+s.cam.Offset[1]-shake[1]-y),
		kx: s.widthScale * cs * gu, ky: s.heightScale * cs * gu,
		kxPrecise: float64(s.widthScale) * float64(cs) * float64(gu),
		kyPrecise: float64(s.heightScale) * float64(cs) * float64(gu),
	}
	ovCapture = f
	ovCaptureDebug = false
	ovCaptureThread.Store(ovCurrentThreadID())
	defer func() {
		ovCaptureThread.Store(0)
		ovCapture = nil
	}()
	if s.stage == nil {
		return
	}
	s.draw(x, y, scl)
	if debug && s.clsnDisplay {
		ovCaptureDebug = true
		s.clsnSpr.ovEnsureTex()
		s.drawTop()
	}
	s.luaDiscardDrawQueue()
}

// (dev, with IKEMEN_TICK_LOG) the publication split, one line a tick: P tick capture_us fill_us lock_us - capture:
// ovCaptureFrame whole; fill: ovFillState; lock: the wait for the publication's write lock (the draw's pin).
func ovPublishDrawSnapshot(s *System) {
	t0 := ovTimingUs()
	f := ovPublishTarget()
	var tc0, tc1 int64
	// The former latest was unpinned when publication acquired the write lock.
	func() {
		if ovFreezeIDs() { // (no char comes or goes during the capture: host_ov_ids.go)
			defer ovThawIDs()
		}
		tc0 = ovTimingUs()
		ovCaptureFrame(s, f, true)
		tc1 = ovTimingUs()
	}()
	f.stateN = ovFillState(f.state[:])
	f.tick = atomic.LoadUint64(&ovTicks)
	f.buildUs = ovTimingUs() - t0
	f.simulationUs = 0
	if ovSimulationStarted != 0 {
		f.tick++ // boot's initial snapshot is tick 0; only a woken tick advances
		f.simulationUs = ovTimingUs() - ovSimulationStarted
	}
	tl0 := ovTimingUs()
	ovPublishSet(f)
	if ovTickLog != nil {
		tl1 := ovTimingUs()
		ovTickLog.line('P', int64(f.tick), tc1-tc0, tl0-tc1, tl1-tl0)
	}
}

// ovPublishTarget: the pooled buffer the next capture writes - never the latest, never one a draw has pinned. (A draw
// pins only the latest, under the read lock; latest changes only under the write lock, on this thread: a draw that
// took a former latest pinned it before that publication returned.)
func ovPublishTarget() *ovDrawSnapshot {
	if !ovPubTriple {
		return &ovSnapshots.buffers[ovSnapshots.next]
	}
	for {
		for i := range ovSnapshots.buffers {
			if f := &ovSnapshots.buffers[i]; f != ovSnapshots.latest && ovSnapshots.pins[i].Load() == 0 {
				return f
			}
		}
		time.Sleep(100 * time.Microsecond) // (never: one draw pins one buffer at a time)
	}
}

// ovPublishSet: f published as the latest (two buffers: after the draw holding the former latest has let it go)
func ovPublishSet(f *ovDrawSnapshot) {
	ovSnapshots.Lock()
	ovSnapshots.latest = f
	ovSnapshots.next ^= 1
	ovSnapshots.Unlock()
}

// ovPinLatest: (the draw) the latest snapshot pinned until unpin is called - three buffers: its read lock held only
// to pin it; two: held until unpin (the old barrier)
func ovPinLatest() (f *ovDrawSnapshot, unpin func()) {
	ovSnapshots.RLock()
	f = ovSnapshots.latest
	if !ovPubTriple || f == nil {
		return f, ovSnapshots.RUnlock
	}
	for i := range ovSnapshots.buffers {
		if f == &ovSnapshots.buffers[i] {
			p := &ovSnapshots.pins[i]
			p.Add(1)
			ovSnapshots.RUnlock()
			return f, func() { p.Add(-1) }
		}
	}
	return f, ovSnapshots.RUnlock
}

// Recorder seam: an owned copy can outlive the publication lock and both pooled render buffers. Reuse dst across
// calls to pool its op storage. Frame context pointers are rebound to dst; strong texture references stay alive.
func ovCopyDrawSnapshot(dst *ovDrawSnapshot) bool {
	if dst == nil || dst == &ovSnapshots.buffers[0] || dst == &ovSnapshots.buffers[1] || dst == &ovSnapshots.buffers[2] {
		return false
	}
	ovSnapshots.RLock()
	defer ovSnapshots.RUnlock()
	src := ovSnapshots.latest
	if src == nil {
		return false
	}
	ops := dst.ops
	for i := range ops {
		ops[i] = ovDrawOp{}
	}
	*dst = *src
	dst.ops, dst.sort = append(ops[:0], src.ops...), nil
	for i := range dst.ops {
		if dst.ops[i].rp.ovFrame != nil {
			dst.ops[i].rp.ovFrame = dst
		}
	}
	return true
}

func ovRenderScreen(rp RenderParams) [4]int32 {
	if rp.ovFrame != nil {
		return rp.ovFrame.screen
	}
	return sys.scrrect
}

// The pipeline's live frame: the newest published snapshot, pinned while it is drawn
func ovRenderDrawSnapshot(fbo uint32, w, h int32, flags int, vp, params [16]float32) int {
	ovSnapshotDebug.Store(int32(Btoi(flags&ovFlagDebugClsn != 0)))
	r, ok := gfx.(*Renderer_GL33)
	if !ok {
		ovSetError("overlay rendering needs the OpenGL 3.3 renderer")
		return -1
	}
	f, unpin := ovPinLatest()
	defer unpin()
	if f == nil {
		return 0
	}
	// Publication can happen after the API's first drain. Pin the chosen frame before the final drain so all uploads
	// queued by its capture are complete before IsValid/texture sampling on this same GL thread.
	ovDrainTasks()
	return ovDrawFrozen(r, f, fbo, w, h, flags, vp, params)
}

// A replayed clip (its recorded draw list, camera, PalFX and shader clock swapped in by ovReplayRenderState) is
// captured on this thread into the replay's own snapshot while the world holds (no tick is pending during a replay,
// the Ikemen thread is parked), then drawn by the same frozen renderer as the pipeline's live frames.
var ovReplaySnap ovDrawSnapshot

func ovRenderReplay(s *System, fbo uint32, w, h int32, flags int, vp, params [16]float32) int {
	r, ok := gfx.(*Renderer_GL33)
	if !ok {
		ovSetError("overlay rendering needs the OpenGL 3.3 renderer")
		return -1
	}
	if ovStepPending {
		return 0 // (never: replay_begin joins first and no tick begins while a clip plays)
	}
	func() { // (restored by defer: a capture panic, recovered by ikemen_render, must not leave the replay's list, camera and
		// PalFX swapped into the live world)
		restore := ovReplayRenderState(s)
		defer restore()
		ovCaptureFrame(s, &ovReplaySnap, false)
	}()
	ovDrainTasks()
	return ovDrawFrozen(r, &ovReplaySnap, fbo, w, h, flags, vp, params)
}

// ovFrozenCard: the card set up from the snapshot and the host's camera for a w x h target - the frozen renderer's
// frame set-up (ovRenderLiveFrame's, with the snapshot's card origin and scale)
func ovFrozenCard(r *Renderer_GL33, f *ovDrawSnapshot, w, h int32, flags int, vp, params [16]float32) {
	ovCard = f.card
	c := &ovCard
	c.setCamera(vp, params, flags)
	c.scrW, c.scrH = float32(f.screen[2]), float32(f.screen[3])
	// The floor rule (setFloor): only against a host's depth, never in the parity profile
	c.setFloor(w, h, r.ovDepthRange, ovFloorSignOf(flags, params))
}

// The frozen renderer: the live renderer's frame set-up (ovRenderLiveFrame) with the snapshot's card origin and
// scale, then the recorded owner steps and draws in order
func ovDrawFrozen(r *Renderer_GL33, f *ovDrawSnapshot, fbo uint32, w, h int32, flags int, vp, params [16]float32) int {
	if f.card.kx == 0 || f.card.ky == 0 {
		return 0
	}
	r.ovBeginFrame(fbo, w, h, flags, params[6] == 1)
	defer r.ovEndFrame()
	t0 := time.Now()
	ovFrameDecodes, ovFrameDecodeUs, ovFrameUploads, ovFrameUploadBytes = 0, 0, 0, 0
	ovFrameSprites, ovFrameDraws, ovFrameSame, ovFrameSameButProj, ovFrameGLDraws = 0, 0, 0, 0, 0
	defer ovRenderStats(t0, len(f.ops)) // host-only counters, no live draw-list read
	ovFrozenCard(r, f, w, h, flags, vp, params)
	ovViewCullBegin() // (sprites off the host's view are skipped: host_ov_viewcull.go)
	defer ovViewCullEnd()
	c := &ovCard
	ovPprofCheck()
	ovRendering = true
	defer func() { ovRendering = false }()
	// The late pass (host_ov_depth.go): drawn before the host's transparent objects, the sprites' depth left for them
	if flags&ovFlagLatePass != 0 && ovAltOn("late") && r.ovDepthBegin(fbo, w, h, params[6] == 1) {
		defer r.ovDepthEnd(params[6] == 1)
	}
	for i := range f.ops {
		op := &f.ops[i]
		switch {
		case op.owner:
			c.setOwner(op.z, op.x, op.y, op.floor, op.screen)
			if ovLate && !op.screen {
				c.setUpright(op.z, op.x) // (the depth it leaves for the host: host_ov_depth.go)
			}
			ovFrameSprites++
		case op.debug && flags&ovFlagDebugClsn == 0:
		case op.fill:
			fillRectState(op.rect, op.color, op.alpha, op.fx, f.screen) // (it leaves no depth: ovDepthFill)
		default:
			RenderSprite(op.rp)
		}
	}
	ovGLDirect() // (the fast path's queued GL calls run now, inside the frame's timing - host_ov_render.go)
	return 1
}

// Split rendezvous: only the host calls these, in order. Commands sent while pending join first (safe even for
// spawn/held input, which reserve live slots). State reads during a tick return the previous joined records. Events
// must be polled after end; the API joins on an early poll instead of exposing them.
var ovStepPending bool
var ovStepStarted time.Time
var ovJoinedState [MaxSimul * 2 * ovREC]float32
var ovJoinedStateN int

func ovCopyJoinedState() {
	ovSnapshots.RLock()
	defer ovSnapshots.RUnlock()
	if f := ovSnapshots.latest; f != nil {
		ovJoinedStateN = f.stateN
		copy(ovJoinedState[:], f.state[:f.stateN])
	}
}

func ovStepTimeout() time.Duration {
	d := time.Duration(ovCfg.TickTimeout * float64(time.Second))
	if d <= 0 {
		d = 20 * time.Second
	}
	return d
}

func ovStepBegin() int {
	if !ovCfg.Pipeline {
		ovSetError("step_begin requires config pipeline=true")
		return -1
	}
	if ovStepPending {
		ovSetError("step_begin: tick already pending")
		return -1
	}
	status := atomic.LoadInt32(&ovStatus)
	if status != 1 {
		return int(status)
	}
	if atomic.LoadInt32(&ovPaused) != 0 || ovReplayIsPlaying() { // (the live world holds while a clip plays)
		return 0
	}
	ovCopyJoinedState()
	ovStepStarted = time.Now()
	select {
	case ovWake <- struct{}{}:
		ovStepPending = true
		if ovTickLog != nil { // (dev: IKEMEN_TICK_LOG)
			ovTickLog.line('B', ovTimingUs(), int64(atomic.LoadUint64(&ovTicks)))
		}
		return 1
	case <-ovDead:
		ovFail("Ikemen thread ended")
		return -1
	default:
		ovFail("step_begin: wake queue occupied")
		return -1
	}
}

// The tick is joined (the Ikemen thread is parked until the next begin): the replay recorder takes the completed
// tick's cued draw list and accepted sounds now, and readies recorded textures (their GL work is queued for the GL
// thread: here the host is outside any host GL call)
func ovCompleteStep() int {
	t0 := time.Now()
	ovStepPending = false
	atomic.AddUint64(&ovTicks, 1)
	ovCopyJoinedState()
	ovReplayCapture()
	var captured int64
	if ovTickLog != nil {
		captured = ovTimingUs()
	}
	ovReplayWarm()
	if ovTickLog != nil { // (dev: IKEMEN_TICK_LOG - the join: entered, tick received, capture done, left)
		ovTickLog.line('E', ovTickEnter, ovTickRecv, captured, ovTimingUs(), int64(atomic.LoadUint64(&ovTicks)))
	}
	if simStats != nil { // (dev: IKEMEN_SIM_STATS - the join's own work, host thread)
		atomic.AddInt64(&simStatsJoinNs, int64(time.Since(t0)))
		atomic.AddInt64(&simStatsJoinN, 1)
	}
	return 1
}

func ovStepEnd() int {
	if !ovStepPending {
		return 0
	}
	if ovTickLog != nil { // (dev: IKEMEN_TICK_LOG)
		ovTickEnter = ovTimingUs()
		ovTickRecv = ovTickEnter
	}
	// A host may be suspended/paused long after the tick completed. Consume its queued acknowledgement before
	// considering the begin-time deadline.
	select {
	case <-ovDone:
		return ovCompleteStep()
	default:
	}
	remaining := ovStepTimeout() - time.Since(ovStepStarted)
	if remaining <= 0 {
		remaining = time.Nanosecond
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-ovDone:
		if ovTickLog != nil {
			ovTickRecv = ovTimingUs()
		}
		return ovCompleteStep()
	case <-ovDead:
		ovStepPending = false
		ovFail("Ikemen thread ended during a tick")
		return -1
	case <-timer.C:
		ovFail("one tick took longer than %v", ovStepTimeout())
		return -1
	}
}

func ovStep() int {
	if !ovCfg.Pipeline {
		return ovStepLegacy()
	}
	if ovStepPending {
		return ovStepEnd()
	}
	if n := ovStepBegin(); n != 1 {
		return n
	}
	return ovStepEnd()
}
