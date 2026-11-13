package main

// Overlay: the overlay frame's sprite drawing cost (ikemen_render on the host's thread).
//
// The fast sprite path (default on; env IKEMEN_FAST_DRAW=0 draws every sprite through Ikemen's generic RenderSprite):
// the same sprites in the same order, the same geometry maths (renderWithBlending, renderSpriteQuad and
// renderSpriteHTile run unchanged) and the same GL state at every draw, for far less CPU per sprite:
//   - the sprite shader's uniforms by location (looked up once), each uploaded only when its bits change - the card
//     projection too (it changes between owners, not between one owner's sprites) and the modelview (constant unless
//     a sprite is rotated); x1x2x4x3 only while isTrapez is on, the only time sprite.frag reads it;
//   - the quad's vertices written into a persistently mapped, fenced vertex ring (GL_ARB_buffer_storage; else
//     glBufferSubData at a moving offset) and drawn from their place in it, with the fast path's own vertex array,
//     instead of a new buffer store (glBufferData) per sprite;
//   - textures on fixed units (sprite 0, palette 1): no sampler uniform changes, no rebind of what is already there;
//   - the GL calls queued and run from C in one cgo call (host_ov_glexec.go), consecutive draws with no state change
//     in between as one glMultiDrawArrays (see "The GL command queue" below; env IKEMEN_GL_QUEUE=0 off);
//   - no heap allocation per sprite (no variadic interface calls, no slices of locals handed to cgo); the huge-sprite
//     rule's stand height cached per root fighter, CachePalTex's palette compare one memory compare.
// One value cache per uniform location serves both paths (render_gl33.go SetUniformISub/FSub/Matrix), so a generic
// draw in between (FillRect, a custom shader, subadd's gray) can never leave the fast path believing a stale value.
//
// Dev tools:
//   - overlay.log "render stats" every 600 overlay frames: ovRenderFrame's own time per frame, sprites drawn, draws
//     (passes), GL draw calls, us/sprite, and what changed before the draws; "render totals" at ikemen_shutdown;
//   - env IKEMEN_RENDER_PPROF=<file>: a Go CPU profile from the render frame at Ikemen tick IKEMEN_RENDER_PPROF_AT
//     (default 600) for IKEMEN_RENDER_PPROF_SEC seconds (default 30); go tool pprof -top -focus=ovRenderFrame <file>;
//   - env IKEMEN_RENDER_AB=N: every N-th overlay frame is drawn twice more, by the generic and by the fast path, each into
//     an exact copy of the host's target (colour + depth blitted from it), read back and compared bit for bit
//     (overlay.log "render A/B"); the real frame is drawn afterwards as usual;
//   - env IKEMEN_RENDER_ALT=fast: the generic path on even overlay frames, the fast one on odd frames, timed apart
//     (overlay.log "render ALT": a speed ratio on the same scene and the same machine load);
//   - env IKEMEN_SEED=<n>: Ikemen's RNG seeded at ikemen_init (reproducible ovhost runs for before/after image diffs);
//   - env IKEMEN_RING_MAP=0: the glBufferSubData vertex ring (and no GL command queue) even with buffer storage.

import (
	"math"
	"os"
	"runtime/pprof"
	"strconv"
	"time"
	"unsafe"

	gl "github.com/go-gl/gl/v3.3-core/gl"
	mgl "github.com/go-gl/mathgl/mgl32"
)

// ---------------------------------------------------------------------------------------------------------------------
// The fast sprite path

var ovFastDraw = os.Getenv("IKEMEN_FAST_DRAW") != "0"

// Set while ovRenderSprite runs: drawQuads hands its quad to ovDrawQuads
var ovFastQuad *Renderer_GL33

const (
	ovUniCap     = 64 // sprite-shader uniform locations cached by index (larger ones use Ikemen's maps)
	ovQuadBytes  = 64 // 4 vertices x (x, y, u, v) float32
	ovRingBytes  = 4096 * ovQuadBytes
	ovUniUnknown = 0
	ovUniInt     = 1 // 2..5: 1..4 floats
	ovUniMat4    = 6
)

// The last value uploaded to one uniform location of the sprite program (bits, so -0/+0 and NaNs count as changes)
type ovUniformSlot struct {
	kind uint8
	i    int32
	f    [16]uint32
}

// Notes an int; true when it differs from the GL value (upload it)
func (s *ovUniformSlot) setI(v int32) bool {
	if s.kind == ovUniInt && s.i == v {
		return false
	}
	s.kind, s.i = ovUniInt, v
	return true
}

// Notes 1-4 floats; true when they differ from the GL value (upload them)
func (s *ovUniformSlot) setF(v []float32) bool {
	kind := uint8(1 + len(v))
	if s.kind == kind {
		same := true
		for i, x := range v {
			if s.f[i] != math.Float32bits(x) {
				same = false
				break
			}
		}
		if same {
			return false
		}
	}
	s.kind = kind
	for i, x := range v {
		s.f[i] = math.Float32bits(x)
	}
	return true
}

// A matrix the generic path uploads (always): noted so the fast path knows what the program holds
func (s *ovUniformSlot) noteMat4(v []float32) {
	s.kind = ovUniMat4
	for i := 0; i < 16; i++ {
		s.f[i] = math.Float32bits(v[i])
	}
}

type ovGL33 struct {
	uni [ovUniCap]ovUniformSlot

	// the sprite shader's uniform locations (-1: not in the program)
	locModelview, locProjection, locX1x2x4x3, locAlpha, locTint, locMask, locNeg, locGray int32
	locAdd, locMult, locIsFlat, locIsRgba, locIsTrapez, locHue, locFloor, locTex, locPal  int32
	locDepthTest, locDepthCut, locWorldDepth, locUpright, locUprightAt, locSmooth         int32

	// the fast path's own vertex array (its attributes read vbo), the vertex ring in vbo
	vao, vbo   uint32
	vaoBound   bool           // vao is the bound vertex array (false after anything else binds one)
	activeUnit int32          // the active texture unit as last set (-1: unknown)
	ring       unsafe.Pointer // vbo's persistent, coherent mapping (GL_ARB_buffer_storage); nil: glBufferSubData
	ringPos    int            // next quad of the ring
	fence      [ovRingSegs]uintptr
	ringOff    int  // (no mapping) next free byte of vbo's store; -1: a new store before the next quad
	arrayBound bool // (no mapping) GL_ARRAY_BUFFER is vbo
	tmpMat     [16]float32
	queue      bool // the GL command queue is used (its executor loaded and the ring mapped)
}

const (
	ovRingSegs    = 8
	ovRingSegQuad = 8192 // quads per segment: 512 KB, the whole ring 4 MB (about 40 frames of 1500 draws)
	ovRingQuads   = ovRingSegs * ovRingSegQuad
)

// The quad being drawn (a pointer-free global: cgo's pointer check refuses an unsafe.Pointer into the renderer
// struct, which holds Go pointers)
var ovQuadVerts [16]float32

// After the sprite shader is linked (Renderer_GL33.Init, overlay mode)
func (r *Renderer_GL33) ovInitFast() {
	u := r.spriteShader.uniforms
	loc := func(name string) int32 {
		if l, ok := u[name]; ok {
			return l
		}
		return -1
	}
	o := &r.ov
	o.locModelview, o.locProjection, o.locX1x2x4x3 = loc("modelview"), loc("projection"), loc("x1x2x4x3")
	o.locAlpha, o.locTint, o.locMask, o.locNeg, o.locGray = loc("alpha"), loc("tint"), loc("mask"), loc("neg"), loc("gray")
	o.locAdd, o.locMult, o.locIsFlat, o.locIsRgba = loc("add"), loc("mult"), loc("isFlat"), loc("isRgba")
	o.locIsTrapez, o.locHue, o.locFloor = loc("isTrapez"), loc("hue"), loc("ovFloor")
	o.locDepthTest, o.locDepthCut, o.locWorldDepth = loc("ovDepthTest"), loc("ovDepthCut"), loc("ovWorldDepth")
	o.locUpright, o.locUprightAt, o.locSmooth = loc("ovUpright"), loc("ovUprightAt"), loc("ovSmooth")
	// (a sampler's location: Ikemen's SetTexture reads the map without the ok check, so a missing one means 0)
	o.locTex, o.locPal = u["tex"], u["pal"]
	o.ringOff = -1
	o.activeUnit = -1
	for i := range o.uni {
		o.uni[i].kind = ovUniUnknown
	}
	// The fast path's vertex array: spriteVAO's layout (position at 0, uv at 8, stride 16) over its own buffer
	gl.GenVertexArrays(1, &o.vao)
	gl.GenBuffers(1, &o.vbo)
	gl.BindVertexArray(o.vao)
	gl.BindBuffer(gl.ARRAY_BUFFER, o.vbo)
	storage := ovHasExtension("GL_ARB_buffer_storage") && os.Getenv("IKEMEN_RING_MAP") != "0"
	if storage {
		const flags = gl.MAP_WRITE_BIT | gl.MAP_PERSISTENT_BIT | gl.MAP_COHERENT_BIT
		gl.BufferStorage(gl.ARRAY_BUFFER, ovRingQuads*ovQuadBytes, nil, flags)
		o.ring = gl.MapBufferRange(gl.ARRAY_BUFFER, 0, ovRingQuads*ovQuadBytes, flags)
	}
	if o.ring == nil {
		gl.BufferData(gl.ARRAY_BUFFER, ovRingBytes, nil, gl.STREAM_DRAW)
	}
	locPos := uint32(r.spriteShader.attributes["position"])
	gl.EnableVertexAttribArray(locPos)
	gl.VertexAttribPointerWithOffset(locPos, 2, gl.FLOAT, false, 16, 0)
	locUV := uint32(r.spriteShader.attributes["uv"])
	gl.EnableVertexAttribArray(locUV)
	gl.VertexAttribPointerWithOffset(locUV, 2, gl.FLOAT, false, 16, 8)
	gl.BindVertexArray(0)
	gl.BindBuffer(gl.ARRAY_BUFFER, 0)
	o.queue = o.ring != nil && os.Getenv("IKEMEN_GL_QUEUE") != "0" && ovGLExecInit()
	LogMessage("[overlay] fast sprite path %v: vertex ring %s, GL command queue %v", ovFastDraw,
		map[bool]string{true: "persistently mapped (4 MB, 8 fenced segments)", false: "glBufferSubData"}[o.ring != nil],
		o.queue)
}

// Whether the current GL context lists an extension
func ovHasExtension(name string) bool {
	var n int32
	gl.GetIntegerv(gl.NUM_EXTENSIONS, &n)
	for i := int32(0); i < n; i++ {
		if p := gl.GetStringi(gl.EXTENSIONS, uint32(i)); p != nil && gl.GoStr(p) == name {
			return true
		}
	}
	return false
}

// Something other than the fast path bound a vertex array or buffer or changed the active texture unit
func (r *Renderer_GL33) ovForgetBindings() {
	r.ov.vaoBound = false
	r.ov.activeUnit = -1
	r.ov.arrayBound = false
}

// Switches between the generic and the fast path (A/B): every cached uniform value is forgotten
func ovSetFastDraw(on bool) {
	if on == ovFastDraw {
		return
	}
	ovGLDirect()
	ovFastDraw = on
	if r, ok := gfx.(*Renderer_GL33); ok {
		for i := range r.ov.uni {
			r.ov.uni[i].kind = ovUniUnknown
		}
		r.ov.ringOff = -1
	}
}

// A matrix uniform of the sprite program, uploaded only when its bits change
func (r *Renderer_GL33) ovUniMat4(loc int32, m *[16]float32) {
	if loc < 0 {
		return
	}
	if loc >= ovUniCap {
		r.ov.tmpMat = *m
		ovGLDirect()
		gl.UniformMatrix4fv(loc, 1, false, &r.ov.tmpMat[0])
		return
	}
	s := &r.ov.uni[loc]
	mb := (*[16]uint32)(unsafe.Pointer(m))
	if s.kind == ovUniMat4 && s.f == *mb {
		return
	}
	s.kind, s.f = ovUniMat4, *mb
	if loc == r.ov.locProjection {
		ovProjGen++
		ovChg[1]++
	} else {
		ovStateGen++
		ovChg[2]++
	}
	if ovQueueing {
		ovOpM4(loc, &s.f)
		return
	}
	ovGLDirect()
	gl.UniformMatrix4fv(loc, 1, false, (*float32)(unsafe.Pointer(&s.f[0])))
}

// Texture units the overlay may bind: the host's GL state guard (the Godot module's gl_state_guard.h,
// IkemenGLStateGuard::TEXTURE_UNITS) saves and restores the GL_TEXTURE_2D binding of the low units only. Ikemen's
// generic sprite path spreads its LRU texture cache (SetTextureSub) over every unit the GPU has
// (GL_MAX_TEXTURE_IMAGE_UNITS), which includes units a host keeps its own textures on (Godot: the highest ones);
// in overlay mode that cache keeps to units 0-7. Every other overlay binding is already inside them:
// the fast path uses units 0 and 1, uploads unit 0, a custom shader's fixed units are its sampler index (< 5), models
// and post-processing are off.
const ovTexUnits = 8

func (r *Renderer_GL33) ovClampTextureUnits() {
	all := len(r.texCacheTexSerial)
	if all > ovTexUnits {
		r.texCacheTexSerial = r.texCacheTexSerial[:ovTexUnits]
		r.texCacheLastUsed = r.texCacheLastUsed[:ovTexUnits]
	}
	LogMessage("[overlay] sprite texture cache: units 0-%d of the GPU's %d", len(r.texCacheTexSerial)-1, all)
}

// A texture on a given unit (units 0 and 1 of the shared unit cache texCacheTexSerial, kept truthful for the LRU)
func (r *Renderer_GL33) ovBindUnit(unit int32, t *Texture_GL33) {
	r.texCacheTimer++
	if r.texCacheTexSerial[unit] == t.serial {
		r.texCacheLastUsed[unit] = r.texCacheTimer
		return
	}
	if !ovQueueing {
		ovGLDirect()
	}
	if r.ov.activeUnit != unit {
		if ovQueueing {
			ovOp2(ovOpActive, gl.TEXTURE0+uint32(unit))
		} else {
			gl.ActiveTexture(gl.TEXTURE0 + uint32(unit))
		}
		r.ov.activeUnit = unit
	}
	if ovQueueing {
		ovOp2(ovOpBindTex, t.handle)
	} else {
		gl.BindTexture(gl.TEXTURE_2D, t.handle)
	}
	ovStateGen++
	ovChg[0]++
	r.texCacheTexSerial[unit] = t.serial
	r.texCacheLastUsed[unit] = r.texCacheTimer
}

// RenderSprite for the overlay with Ikemen's own sprite shader (rp already checked and set up by RenderSprite):
// the same uniforms, textures and blend passes, set through the caches above
func ovRenderSprite(r *Renderer_GL33, rp *RenderParams) {
	spfx := ShaderPalFX{mult: [3]float32{1, 1, 1}}
	screen := sys.scrrect
	if rp.ovFrame != nil { // (a frozen snapshot's op: its PalFX resolved at capture, its frame's screen)
		spfx, screen = rp.ovFx, rp.ovFrame.screen
	} else if rp.pfx != nil {
		spfx = rp.pfx.getFinalPalFx(rp.blendMode, rp.blendAlpha)
	}
	tint := [4]float32{
		float32(rp.tint&0xff) / 255,
		float32(rp.tint>>8&0xff) / 255,
		float32(rp.tint>>16&0xff) / 255,
		float32(rp.tint>>24&0xff) / 255,
	}
	ovSpriteProjection()                         // (ovCard.cur valid)
	if ovSpriteOffView(rp, float32(screen[3])) { // (off the host's view: host_ov_viewcull.go)
		ovFrameCulled++
		return
	}
	r.SetSpritePipeline("")
	ovQueueing = r.ov.queue // (from here on the GL calls are queued - see the GL command queue below)
	o := &r.ov
	r.ovUniMat4(o.locProjection, (*[16]float32)(&ovCard.cur))
	r.SetUniformISub(o.locIsFlat, 0)
	r.SetUniformISub(o.locMask, rp.mask)
	r.SetUniformISub(o.locIsTrapez, Btoi(Abs(Abs(rp.xts)-Abs(rp.xbs)) > 0.001))
	r.SetUniformFSub(o.locGray, spfx.gray)
	r.SetUniformFSub(o.locHue, spfx.hue)
	r.SetUniformFSub(o.locTint, tint[0], tint[1], tint[2], tint[3])
	if o.locFloor >= 0 {
		fl := ovFloorUniform()
		r.SetUniformFSub(o.locFloor, fl[0], fl[1], fl[2], fl[3])
	}
	if o.locSmooth >= 0 {
		r.SetUniformISub(o.locSmooth, ovSmoothNow())
	}
	r.SetUniformISub(o.locIsRgba, Btoi(rp.paltex == nil))
	r.ovDepthSprite() // (the late pass's own depth test: host_ov_depth.go)
	// textures on fixed units (the palette first, so the unit left active is the sprite's): the sampler uniforms then
	// stay put, and a texture already on its unit is not bound again
	if rp.paltex != nil {
		r.ovBindUnit(1, rp.paltex.(*Texture_GL33))
		r.SetUniformISub(o.locPal, 1)
	}
	r.ovBindUnit(0, rp.tex.(*Texture_GL33))
	r.SetUniformISub(o.locTex, 0)
	modelview := mgl.Translate3D(0, float32(screen[3]), 0)
	renderPass := func(eq BlendEquation, src, dst BlendFunc, a float32) {
		r.EnableBlending(eq, src, dst)
		r.SetUniformISub(o.locNeg, Btoi(spfx.neg))
		r.SetUniformFSub(o.locAdd, spfx.add[0], spfx.add[1], spfx.add[2])
		r.SetUniformFSub(o.locMult, spfx.mult[0], spfx.mult[1], spfx.mult[2])
		r.SetUniformFSub(o.locAlpha, a)
		renderSpriteQuad(modelview, *rp)
	}
	ovFastQuad = r
	ovRenderBlend(renderPass, rp, &spfx) // (renderWithBlending; in the late pass an opaque pass leaves its depth)
	ovFastQuad = nil
	ovQueueing = false // (still queued calls run before the next direct GL call: ovGLDirect)
	r.DisableScissor()
}

// drawQuads for the fast path: the modelview and x1x2x4x3 through the caches, the quad into the vertex ring
func ovDrawQuads(r *Renderer_GL33, modelview *mgl.Mat4, x1, y1, x2, y2, x3, y3, x4, y4 float32) {
	o := &r.ov
	r.ovUniMat4(o.locModelview, (*[16]float32)(modelview))
	// (sprite.frag reads x1x2x4x3 only while isTrapez is on)
	if t := o.locIsTrapez; t < 0 || t >= ovUniCap || o.uni[t].kind != ovUniInt || o.uni[t].i != 0 {
		r.SetUniformFSub(o.locX1x2x4x3, x1, x2, x4, x3)
	}
	uvBias := float32(0.000002) // (drawQuads: Ikemen-GO issue 3583)
	q := &ovQuadVerts
	q[0], q[1], q[2], q[3] = x2, y2, 1, 1-uvBias
	q[4], q[5], q[6], q[7] = x3, y3, 1, 0
	q[8], q[9], q[10], q[11] = x1, y1, uvBias, 1-uvBias
	q[12], q[13], q[14], q[15] = x4, y4, uvBias, 0
	r.ovDrawQuad()
}

// Draws ovQuadVerts as a triangle strip from the vertex ring (the same 4 vertices drawQuads uploads, drawn from
// their place in the ring instead of from the start of a new buffer store)
func (r *Renderer_GL33) ovDrawQuad() {
	o := &r.ov
	if !o.vaoBound {
		if ovQueueing {
			ovOp2(ovOpBindVAO, o.vao)
		} else {
			ovGLDirect()
			gl.BindVertexArray(o.vao)
		}
		o.vaoBound = true
	}
	var first int32
	if o.ring != nil {
		if o.ringPos%ovRingSegQuad == 0 {
			r.ovRingSegment(o.ringPos / ovRingSegQuad)
		}
		*(*[16]float32)(unsafe.Add(o.ring, o.ringPos*ovQuadBytes)) = ovQuadVerts
		first = int32(o.ringPos * 4)
		if o.ringPos++; o.ringPos == ovRingQuads {
			o.ringPos = 0
		}
	} else {
		ovGLDirect()
		if !o.arrayBound {
			gl.BindBuffer(gl.ARRAY_BUFFER, o.vbo)
			o.arrayBound = true
		}
		if o.ringOff < 0 || o.ringOff+ovQuadBytes > ovRingBytes {
			gl.BufferData(gl.ARRAY_BUFFER, ovRingBytes, nil, gl.STREAM_DRAW) // (a fresh store: queued draws keep the old one)
			o.ringOff = 0
		}
		gl.BufferSubData(gl.ARRAY_BUFFER, o.ringOff, ovQuadBytes, unsafe.Pointer(&ovQuadVerts[0]))
		first = int32(o.ringOff / 16)
		o.ringOff += ovQuadBytes
	}
	if ovQueueing {
		ovOp2(ovOpDraw, uint32(first))
	} else {
		ovGLDirect()
		gl.DrawArrays(gl.TRIANGLE_STRIP, first, 4)
		ovFrameGLDraws++
	}
	ovFrameDraws++
	if ovStateGen == ovLastStateGen {
		if ovProjGen == ovLastProjGen {
			ovFrameSame++
		} else {
			ovFrameSameButProj++
		}
	}
	ovLastStateGen, ovLastProjGen = ovStateGen, ovProjGen
	for i := range ovChg {
		if ovChg[i] != ovLastChg[i] {
			ovFrameChg[i]++
		}
	}
	ovLastChg = ovChg
}

// Render stats: state changes by kind (texture binds, card projection, other uniforms, blend) and the draws after each
var ovChg, ovLastChg [4]uint64
var ovFrameChg [4]int
var ovStatChg [4]int64

// The ring enters segment seg: the segment before it gets a fence behind its last draw, and seg's own fence (set when
// the ring last left it, a whole ring ago) is waited for, so no vertex the GPU may still read is overwritten
func (r *Renderer_GL33) ovRingSegment(seg int) {
	ovGLDirect() // (every draw of the segment before is issued before its fence)
	o := &r.ov
	prev := (seg + ovRingSegs - 1) % ovRingSegs
	if o.fence[prev] == 0 {
		o.fence[prev] = gl.FenceSync(gl.SYNC_GPU_COMMANDS_COMPLETE, 0)
	}
	if f := o.fence[seg]; f != 0 {
		gl.ClientWaitSync(f, gl.SYNC_FLUSH_COMMANDS_BIT, 5e9)
		gl.DeleteSync(f)
		o.fence[seg] = 0
	}
}

// Render stats: GL state changes (uniform uploads, texture binds, blend changes; projection uploads apart) and the
// draws that changed nothing but their quad (or only the card projection) since the draw before
var ovStateGen, ovProjGen, ovLastStateGen, ovLastProjGen uint64
var ovFrameSame, ovFrameSameButProj int
var ovStatSame, ovStatSameButProj int64

// ---------------------------------------------------------------------------------------------------------------------
// The GL command queue. While ovRenderSprite runs (ovQueueing), the fast path's GL calls - uniforms, texture binds,
// blend, its vertex array, draws - are not made one cgo call each but queued here; host_ov_glexec.go runs them from
// C in one call, the same calls with the same arguments in the same order, and hands consecutive draws with nothing
// in between (the same state) to one glMultiDrawArrays. The queue runs (ovGLDirect) before any GL call made directly:
// every such path of the GL33 renderer that an overlay frame can reach starts with ovGLDirect (texture uploads through
// SetActiveTexture0, program changes, the generic uniform/blend/scissor/texture/vertex/draw calls, frame begin and
// end, the ring's fences, the A/B readbacks), so no direct call ever overtakes a queued one. Env IKEMEN_GL_QUEUE=0 off.

const ovOpsCap = 1 << 15

// Op codes (host_ov_glexec.go MK_*)
const (
	ovOpU1I uint32 = iota + 1
	ovOpU1F
	ovOpU2F
	ovOpU3F
	ovOpU4F
	ovOpUM4
	ovOpActive
	ovOpBindTex
	ovOpEnable
	ovOpBlendEq
	ovOpBlendFunc
	ovOpBindVAO
	ovOpDraw
	ovOpDepthMask
)

var ovOps [ovOpsCap]uint32 // (a pointer-free global: handed to C as is)
var ovOpsN int
var ovQueueing bool
var ovFrameGLDraws int // GL draw calls made this frame (a multi-draw counts once)

// Runs the queued GL calls, if any (before every direct GL call)
func ovGLDirect() {
	if ovOpsN > 0 {
		ovRunQueue()
	}
}

func ovRunQueue() {
	n := ovGLExec(&ovOps[0], ovOpsN)
	ovOpsN = 0
	if n < 0 {
		LogMessage("[overlay] GL queue: bad op at word %d", -1-n)
		return
	}
	ovFrameGLDraws += n
}

func ovOpRoom(k int) {
	if ovOpsN+k > ovOpsCap {
		ovRunQueue()
	}
}

func ovOp2(a, b uint32) {
	ovOpRoom(2)
	ovOps[ovOpsN], ovOps[ovOpsN+1] = a, b
	ovOpsN += 2
}

func ovOp3(a, b, c uint32) {
	ovOpRoom(3)
	ovOps[ovOpsN], ovOps[ovOpsN+1], ovOps[ovOpsN+2] = a, b, c
	ovOpsN += 3
}

// glUniform{1,2,3,4}f
func ovOpUF(loc int32, v []float32) {
	ovOpRoom(2 + len(v))
	ovOps[ovOpsN] = ovOpU1F + uint32(len(v)-1)
	ovOps[ovOpsN+1] = uint32(loc)
	for i, x := range v {
		ovOps[ovOpsN+2+i] = math.Float32bits(x)
	}
	ovOpsN += 2 + len(v)
}

// glUniformMatrix4fv (column-major, no transpose), the matrix as float bits
func ovOpM4(loc int32, m *[16]uint32) {
	ovOpRoom(18)
	ovOps[ovOpsN] = ovOpUM4
	ovOps[ovOpsN+1] = uint32(loc)
	copy(ovOps[ovOpsN+2:ovOpsN+18], m[:])
	ovOpsN += 18
}

// ovPalMemoOn: Sprite.CachePalTex skips the compare when it is handed the very palette slice its copy was
// taken from (env IKEMEN_PALMEMO=0: the compare every time)
var ovPalMemoOn = os.Getenv("IKEMEN_PALMEMO") != "0"

// ovPalWrites: palettes written in place since the start (every such writer adds one: Lua's animPaletteSet); a
// CachePalTex memo taken before a write no longer applies
var ovPalWrites uint64

// Whether two colour lists are equal, element for element (Sprite.CachePalTex runs this for every paletted sprite
// drawn: one vectorised memory compare instead of a loop over 256 colours)
func ovU32Equal(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	return unsafe.String((*byte)(unsafe.Pointer(&a[0])), len(a)*4) ==
		unsafe.String((*byte)(unsafe.Pointer(&b[0])), len(b)*4)
}

// ---------------------------------------------------------------------------------------------------------------------
// Statistics and the CPU profile

var ovFrameSprites, ovFrameDraws int // this overlay frame: sprites drawn, glDrawArrays calls
var ovStatFrames, ovStatSprites, ovStatDraws, ovStatUs, ovStatMaxUs int64
var ovStatGLDraws int64
var ovAllFrames, ovAllSprites, ovAllDraws, ovAllUs int64

// Called once per overlay frame that drew (render thread)
func ovRenderStatsAdd(us int64) {
	if ovAltMode != "" {
		ovAltAdd(us)
	}
	ovStatFrames++
	ovStatSprites += int64(ovFrameSprites)
	ovStatDraws += int64(ovFrameDraws)
	ovStatCulled += int64(ovFrameCulled)
	ovStatGLDraws += int64(ovFrameGLDraws)
	for i := range ovFrameChg {
		ovStatChg[i] += int64(ovFrameChg[i])
		ovFrameChg[i] = 0
	}
	ovStatSame += int64(ovFrameSame)
	ovStatSameButProj += int64(ovFrameSameButProj)
	ovStatUs += us
	if us > ovStatMaxUs {
		ovStatMaxUs = us
	}
	if ovStatFrames >= 600 {
		ovRenderStatsLog("render stats", ovStatFrames, ovStatSprites, ovStatDraws, ovStatUs, ovStatMaxUs)
		if ovStatDraws > 0 {
			d := 100 / float64(ovStatDraws)
			LogMessage("[overlay]   GL draw calls %.1f/frame; draws that changed only their quad: %.1f%%, only quad + card projection: %.1f%%; draws after a texture bind %.1f%%, projection %.1f%%, other uniforms %.1f%%, blend %.1f%%",
				float64(ovStatGLDraws)/float64(ovStatFrames), float64(ovStatSame)*d, float64(ovStatSameButProj)*d,
				float64(ovStatChg[0])*d, float64(ovStatChg[1])*d, float64(ovStatChg[2])*d, float64(ovStatChg[3])*d)
		}
		LogMessage("[overlay]   off the view, not drawn: %.1f sprites/frame", float64(ovStatCulled)/float64(ovStatFrames))
		ovStatCulled = 0
		ovStatGLDraws = 0
		ovStatChg = [4]int64{}
		ovStatSame, ovStatSameButProj = 0, 0
		ovAllFrames += ovStatFrames
		ovAllSprites += ovStatSprites
		ovAllDraws += ovStatDraws
		ovAllUs += ovStatUs
		ovStatFrames, ovStatSprites, ovStatDraws, ovStatUs, ovStatMaxUs = 0, 0, 0, 0, 0
	}
}

func ovRenderStatsLog(what string, frames, sprites, draws, us, maxUs int64) {
	if frames <= 0 {
		return
	}
	perSprite := float64(0)
	if sprites > 0 {
		perSprite = float64(us) / float64(sprites)
	}
	LogMessage("[overlay] %s (tick %d, fast %v): %d frames, %.0f us/frame (max %d), %.1f sprites/frame, %.2f us/sprite, %.1f draws/frame",
		what, ovTicks, ovFastDraw, frames, float64(us)/float64(frames), maxUs, float64(sprites)/float64(frames), perSprite,
		float64(draws)/float64(frames))
}

// The whole run's numbers (ikemen_shutdown)
func ovRenderStatsTotal() {
	if ovAltMode != "" {
		var us, sp, fr [2]int64
		for i := 0; i < 2; i++ {
			us[i], sp[i], fr[i] = ovAltAllUs[i]+ovAltUs[i], ovAltAllSprites[i]+ovAltSprites[i], ovAltAllFrames[i]+ovAltFrames[i]
		}
		ovAltLog("render ALT totals", us, sp, fr)
	}
	ovRenderStatsLog("render totals", ovAllFrames+ovStatFrames, ovAllSprites+ovStatSprites, ovAllDraws+ovStatDraws,
		ovAllUs+ovStatUs, ovStatMaxUs)
	if ovAB.frames > 0 || ovAB.failed != "" {
		LogMessage("[overlay] render A/B totals: %d frames compared, %d with differences, %d pixels differ, %d pixels drawn%s",
			ovAB.frames, ovAB.diffFrames, ovAB.diffPixels, ovAB.touched, map[bool]string{true: ", stopped: " + ovAB.failed, false: ""}[ovAB.failed != ""])
	}
}

// Dev: env IKEMEN_RENDER_ALT=fast - that feature (the fast sprite path) off on even overlay frames and on on odd ones, timed
// apart (consecutive frames draw nearly the same sprites, and whatever else loads the machine loads both halves):
// overlay.log "render ALT" every 600 frames and at shutdown. =late: the late pass's depth (host_ov_depth.go; off: the
// frame drawn where the late pass draws it, tested against the scene as before, leaving no depth)
var ovAltMode = os.Getenv("IKEMEN_RENDER_ALT")
var ovAltN uint64
var ovAltUs, ovAltSprites, ovAltDraws, ovAltFrames [2]int64
var ovAltAllUs, ovAltAllSprites, ovAltAllFrames [2]int64

// Before an overlay frame draws: the feature for this frame
func ovAltFrame() {
	if ovAltMode == "" {
		return
	}
	on := ovAltN%2 == 1
	switch ovAltMode {
	case "fast":
		ovSetFastDraw(on)
	}
}

// ovAltOn: whether the frame being drawn has that feature (mode) on - always, unless the A/B is on it
func ovAltOn(mode string) bool {
	return ovAltMode != mode || ovAltN%2 == 1
}

func ovAltAdd(us int64) {
	v := ovAltN % 2
	ovAltN++
	ovAltUs[v] += us
	ovAltSprites[v] += int64(ovFrameSprites)
	ovAltDraws[v] += int64(ovFrameDraws)
	ovAltFrames[v]++
	if ovAltFrames[0]+ovAltFrames[1] >= 600 {
		ovAltLog("render ALT", ovAltUs, ovAltSprites, ovAltFrames)
		for i := 0; i < 2; i++ {
			ovAltAllUs[i] += ovAltUs[i]
			ovAltAllSprites[i] += ovAltSprites[i]
			ovAltAllFrames[i] += ovAltFrames[i]
		}
		ovAltUs, ovAltSprites, ovAltDraws, ovAltFrames = [2]int64{}, [2]int64{}, [2]int64{}, [2]int64{}
	}
}

func ovAltLog(what string, us, sprites, frames [2]int64) {
	per := func(i int) float64 {
		if sprites[i] == 0 {
			return 0
		}
		return float64(us[i]) / float64(sprites[i])
	}
	fr := func(i int) float64 { return float64(Max(frames[i], 1)) }
	LogMessage("[overlay] %s %s (tick %d): off %.0f us/frame %.1f sprites/frame %.2f us/sprite | on %.0f us/frame %.1f sprites/frame %.2f us/sprite",
		what, ovAltMode, ovTicks, float64(us[0])/fr(0), float64(sprites[0])/fr(0), per(0), float64(us[1])/fr(1),
		float64(sprites[1])/fr(1), per(1))
}

var ovPprofPath = os.Getenv("IKEMEN_RENDER_PPROF")
var ovPprofOut *os.File
var ovPprofUntil time.Time

// Starts / stops the dev CPU profile (render thread, start of every overlay frame)
func ovPprofCheck() {
	if ovPprofPath == "" {
		return
	}
	if ovPprofOut == nil {
		at := uint64(600)
		if v, err := strconv.Atoi(os.Getenv("IKEMEN_RENDER_PPROF_AT")); err == nil && v >= 0 {
			at = uint64(v)
		}
		if ovTicks < at {
			return
		}
		f, err := os.Create(ovPprofPath)
		if err != nil {
			LogMessage("[overlay] CPU profile %s: %v", ovPprofPath, err)
			ovPprofPath = ""
			return
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			LogMessage("[overlay] CPU profile: %v", err)
			f.Close()
			ovPprofPath = ""
			return
		}
		sec := 30
		if v, err := strconv.Atoi(os.Getenv("IKEMEN_RENDER_PPROF_SEC")); err == nil && v > 0 {
			sec = v
		}
		ovPprofOut = f
		ovPprofUntil = time.Now().Add(time.Duration(sec) * time.Second)
		LogMessage("[overlay] CPU profile started at tick %d for %d s: %s", ovTicks, sec, ovPprofPath)
		return
	}
	if time.Now().After(ovPprofUntil) {
		ovPprofStop()
	}
}

func ovPprofStop() {
	if ovPprofOut == nil {
		return
	}
	pprof.StopCPUProfile()
	ovPprofOut.Close()
	ovPprofOut = nil
	LogMessage("[overlay] CPU profile written at tick %d: %s", ovTicks, ovPprofPath)
	ovPprofPath = ""
}

// Dev: env IKEMEN_SEED (ikemen_init, before the Ikemen thread starts)
func ovDevSeed() {
	if v := os.Getenv("IKEMEN_SEED"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil {
			ovSeedValue = int32(n)
			Srand(ovSeedValue)
			LogMessage("[overlay] dev seed %d", n)
		}
	}
	if v := os.Getenv("IKEMEN_SEED_AT"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil && n > 0 {
			ovSeedAt = int32(n)
		}
	}
}

// Seeding once at the start is not enough to make two runs the same. Every random() a fighter's states make moves the
// seed on, including the ones made while the fighters' art is still loading - and loading takes a different number of
// ticks each run, because it is spent against a wall-clock budget. Two runs that happened to load in the same number
// of ticks came out bit for bit identical; every pair that did not, differed. So the seed is laid down AGAIN at a
// fixed tick, after loading is certainly over and before anything worth measuring happens: from there the two runs
// share a seed and stay in step. Env IKEMEN_SEED_AT=<tick>, with IKEMEN_SEED.
var (
	ovSeedValue int32
	ovSeedAt    int32
)

// ovSeedStep: at each tick, the one that was asked for lays the seed down again (ovBeforeTick)
func ovSeedStep() {
	if ovSeedAt == 0 || int32(ov.tick) != ovSeedAt {
		return
	}
	Srand(ovSeedValue)
	ovLog("dev seed %d laid down again at tick %d: from here two runs of a scene share their dice",
		ovSeedValue, ovSeedAt)
}

// ---------------------------------------------------------------------------------------------------------------------
// Render A/B (env IKEMEN_RENDER_AB=N)

var ovABEvery, _ = strconv.Atoi(os.Getenv("IKEMEN_RENDER_AB"))
var ovABCount int

var ovAB struct {
	host, fbo, color, depth uint32
	w, h                    int32
	stencil                 bool
	failed                  string
	pre, a, b               []float32
	frames, diffFrames      int
	diffPixels, touched     int64
}

func ovABDue() bool {
	if ovABEvery <= 0 || ovAB.failed != "" {
		return false
	}
	ovABCount++
	return ovABCount%ovABEvery == 0
}

func ovABFail(msg string) {
	ovAB.failed = msg
	LogMessage("[overlay] render A/B off: %s", msg)
}

// The internal format (and sample count) of one attachment of the FBO bound to GL_FRAMEBUFFER
func (r *Renderer_GL33) ovAttachmentFormat(attach uint32) (format, samples int32, ok bool) {
	var typ, name, level int32
	gl.GetFramebufferAttachmentParameteriv(gl.FRAMEBUFFER, attach, gl.FRAMEBUFFER_ATTACHMENT_OBJECT_TYPE, &typ)
	gl.GetFramebufferAttachmentParameteriv(gl.FRAMEBUFFER, attach, gl.FRAMEBUFFER_ATTACHMENT_OBJECT_NAME, &name)
	switch typ {
	case gl.TEXTURE:
		gl.GetFramebufferAttachmentParameteriv(gl.FRAMEBUFFER, attach, gl.FRAMEBUFFER_ATTACHMENT_TEXTURE_LEVEL, &level)
		r.SetActiveTexture0()
		gl.BindTexture(gl.TEXTURE_2D, uint32(name))
		gl.GetTexLevelParameteriv(gl.TEXTURE_2D, level, gl.TEXTURE_INTERNAL_FORMAT, &format)
		gl.GetTexLevelParameteriv(gl.TEXTURE_2D, level, gl.TEXTURE_SAMPLES, &samples)
		gl.BindTexture(gl.TEXTURE_2D, 0)
		for gl.GetError() != gl.NO_ERROR { // (not a 2D texture: no format)
			format = 0
		}
	case gl.RENDERBUFFER:
		gl.BindRenderbuffer(gl.RENDERBUFFER, uint32(name))
		gl.GetRenderbufferParameteriv(gl.RENDERBUFFER, gl.RENDERBUFFER_INTERNAL_FORMAT, &format)
		gl.GetRenderbufferParameteriv(gl.RENDERBUFFER, gl.RENDERBUFFER_SAMPLES, &samples)
	default:
		return 0, 0, false
	}
	return format, samples, format != 0
}

// Scratch target with the host target's formats and size
func (r *Renderer_GL33) ovABEnsure(host uint32, w, h int32) bool {
	st := &ovAB
	if st.fbo != 0 && st.host == host && st.w == w && st.h == h {
		return true
	}
	gl.BindFramebuffer(gl.FRAMEBUFFER, host)
	cf, cs, ok := r.ovAttachmentFormat(gl.COLOR_ATTACHMENT0)
	if !ok || cs > 0 {
		ovABFail("the host's colour attachment is not a single-sample 2D texture or renderbuffer")
		return false
	}
	df, ds, dok := r.ovAttachmentFormat(gl.DEPTH_ATTACHMENT)
	if dok && ds > 0 {
		ovABFail("the host's depth attachment is multisampled")
		return false
	}
	if st.fbo == 0 {
		gl.GenFramebuffers(1, &st.fbo)
		gl.GenRenderbuffers(1, &st.color)
		gl.GenRenderbuffers(1, &st.depth)
	}
	gl.BindRenderbuffer(gl.RENDERBUFFER, st.color)
	gl.RenderbufferStorage(gl.RENDERBUFFER, uint32(cf), w, h)
	gl.BindFramebuffer(gl.FRAMEBUFFER, st.fbo)
	gl.FramebufferRenderbuffer(gl.FRAMEBUFFER, gl.COLOR_ATTACHMENT0, gl.RENDERBUFFER, st.color)
	st.stencil = false
	if dok {
		gl.BindRenderbuffer(gl.RENDERBUFFER, st.depth)
		gl.RenderbufferStorage(gl.RENDERBUFFER, uint32(df), w, h)
		attach := uint32(gl.DEPTH_ATTACHMENT)
		if df == gl.DEPTH24_STENCIL8 || df == gl.DEPTH32F_STENCIL8 {
			attach, st.stencil = gl.DEPTH_STENCIL_ATTACHMENT, true
		}
		gl.FramebufferRenderbuffer(gl.FRAMEBUFFER, attach, gl.RENDERBUFFER, st.depth)
	}
	if status := gl.CheckFramebufferStatus(gl.FRAMEBUFFER); status != gl.FRAMEBUFFER_COMPLETE {
		ovABFail("scratch framebuffer incomplete: 0x" + strconv.FormatUint(uint64(status), 16))
		return false
	}
	st.host, st.w, st.h = host, w, h
	n := int(w) * int(h) * 4
	st.pre, st.a, st.b = make([]float32, n), make([]float32, n), make([]float32, n)
	LogMessage("[overlay] render A/B every %d frames: host target %dx%d colour 0x%x depth 0x%x (stencil %v)", ovABEvery,
		w, h, cf, df, st.stencil)
	return true
}

func ovABRead(buf []float32, w, h int32) {
	ovGLDirect()
	gl.ReadBuffer(gl.COLOR_ATTACHMENT0)
	gl.BindBuffer(gl.PIXEL_PACK_BUFFER, 0)
	gl.PixelStorei(gl.PACK_ALIGNMENT, 4)
	gl.PixelStorei(gl.PACK_ROW_LENGTH, 0)
	gl.PixelStorei(gl.PACK_SKIP_ROWS, 0)
	gl.PixelStorei(gl.PACK_SKIP_PIXELS, 0)
	gl.ReadPixels(0, 0, w, h, gl.RGBA, gl.FLOAT, unsafe.Pointer(&buf[0]))
}

// The frame drawn twice into copies of the host's target (generic path, then fast path) and compared. The caller
// begins the host's frame again afterwards.
func (s *System) ovRenderAB(r *Renderer_GL33, host uint32, w, h int32, flags int, depthLEqual bool, x, y, scl,
	floorSign float32) {
	st := &ovAB
	ovGLDirect()
	if !r.ovABEnsure(host, w, h) {
		return
	}
	keep := ovFastDraw
	var sprites, draws [2]int
	for v := 0; v < 2; v++ {
		ovSetFastDraw(v == 1)
		// an exact copy of the host's target as it is now
		gl.Disable(gl.SCISSOR_TEST)
		gl.ColorMask(true, true, true, true)
		gl.DepthMask(true)
		gl.StencilMask(0xffffffff)
		gl.BindFramebuffer(gl.READ_FRAMEBUFFER, host)
		gl.BindFramebuffer(gl.DRAW_FRAMEBUFFER, st.fbo)
		mask := uint32(gl.COLOR_BUFFER_BIT | gl.DEPTH_BUFFER_BIT)
		if st.stencil {
			mask |= gl.STENCIL_BUFFER_BIT
		}
		gl.BlitFramebuffer(0, 0, w, h, 0, 0, w, h, mask, gl.NEAREST)
		r.ovBeginFrame(st.fbo, w, h, flags, depthLEqual)
		if v == 0 {
			ovABRead(st.pre, w, h)
		}
		ovCard.curValid, ovCard.screen = false, false
		ovCard.z, ovCard.ax, ovCard.ay = 0, 0, 0
		ovCard.setFloor(w, h, r.ovDepthRange, floorSign)
		ovFrameSprites, ovFrameDraws = 0, 0
		ovRendering = true
		s.draw(x, y, scl)
		if flags&ovFlagDebugClsn != 0 {
			s.clsnSpr.ovEnsureTex()
			s.drawTop()
		}
		ovRendering = false
		sprites[v], draws[v] = ovFrameSprites, ovFrameDraws
		gl.BindFramebuffer(gl.FRAMEBUFFER, st.fbo)
		if v == 0 {
			ovABRead(st.a, w, h)
		} else {
			ovABRead(st.b, w, h)
		}
	}
	ovSetFastDraw(keep)
	n := int(w) * int(h)
	pre := unsafe.Slice((*uint32)(unsafe.Pointer(&st.pre[0])), n*4)
	a := unsafe.Slice((*uint32)(unsafe.Pointer(&st.a[0])), n*4)
	b := unsafe.Slice((*uint32)(unsafe.Pointer(&st.b[0])), n*4)
	diff, touched, first := 0, 0, -1
	for i := 0; i < n; i++ {
		j := i * 4
		if a[j] != pre[j] || a[j+1] != pre[j+1] || a[j+2] != pre[j+2] || a[j+3] != pre[j+3] {
			touched++
		}
		if a[j] != b[j] || a[j+1] != b[j+1] || a[j+2] != b[j+2] || a[j+3] != b[j+3] {
			if first < 0 {
				first = i
			}
			diff++
		}
	}
	st.frames++
	st.touched += int64(touched)
	st.diffPixels += int64(diff)
	if diff > 0 {
		st.diffFrames++
		fx, fy := first%int(w), first/int(w)
		LogMessage("[overlay] render A/B frame %d (tick %d): %d sprites, draws %d generic / %d fast: %d of %d pixels DIFFER (first at %d,%d: generic %v fast %v), %d pixels drawn",
			st.frames, ovTicks, sprites[0], draws[0], draws[1], diff, n, fx, fy, st.a[first*4:first*4+4],
			st.b[first*4:first*4+4], touched)
	} else if st.frames <= 5 || st.frames%20 == 0 {
		LogMessage("[overlay] render A/B frame %d (tick %d): %d sprites (fast %d), draws %d generic / %d fast: identical, %d pixels drawn",
			st.frames, ovTicks, sprites[0], sprites[1], draws[0], draws[1], touched)
	}
}
