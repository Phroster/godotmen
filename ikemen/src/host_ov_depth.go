package main

import (
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"

	gl "github.com/go-gl/gl/v3.3-core/gl"
)

// Overlay: the late pass's depth (ikemen_render flag ovFlagLatePass). The host draws its transparent objects - fire,
// smoke, explosions, shadows - after Ikemen's frame and tested against the depth buffer (in Godot: the scene hook's
// transparent phase, engine patch 0003), so whatever of Ikemen's stands in front of them hides them and whatever
// stands behind them lies under them. Ikemen's solid sprites leave their depth for that, while Ikemen's own sprites keep
// MUGEN's layering among themselves (painter order, never their depths): the test against the host's scene is made in
// the sprite shader against a copy of the scene depth taken before any sprite (ovWorldDepth on texture unit
// ovWorldDepthUnit), the hardware's test always passes, and a sprite's opaque pass writes the depth of its solid texels -
// so each pixel ends with the depth of the sprite seen there (the last one drawn) or the scene's.
// Env IKEMEN_LATE_DEPTH=0: no depth left (the host's transparent objects over every sprite). Whether there is a late
// pass at all is the host's choice (the Godot module: IKEMEN_LATE=0 draws Ikemen over all of the host's 3D).

var ovLateDepthOn = os.Getenv("IKEMEN_LATE_DEPTH") != "0"

// Dev (env IKEMEN_LATE_DEPTH=debug): the texels that leave their depth drawn magenta
var ovLateDepthDebug = os.Getenv("IKEMEN_LATE_DEPTH") == "debug"

// The texture unit of the scene depth copy (the module's state guard restores units 0-8: gl_state_guard.h; the
// generic path's texture cache keeps to units 0-7)
const ovWorldDepthUnit = 8

type ovDepthGL struct {
	tex, fb uint32
	w, h    int32
	format  int32
	failed  bool
	note    string
	// the host frame's depth attachment as last read (format 0: none to copy): read again when the frame (fbo, size)
	// changes and every ovDepthReread frames - a read waits for the driver
	src        uint32
	srcW, srcH int32
	srcFormat  int32
	srcAge     int
}

const ovDepthReread = 120

var ovDepth ovDepthGL

// ovLate: the sprites of this frame are drawn by the late pass (ovDepthBegin .. ovDepthEnd); ovLateTest: the shader's
// test then (1 GEQUAL - the host's reverse-Z -, -1 LEQUAL)
var ovLate bool
var ovLateTest int32

// say: a note in the log, once per distinct note
func (d *ovDepthGL) say(note string) {
	if d.note != note {
		d.note = note
		ovLog("late pass: %s", note)
	}
}

// ovDepthBegin: the late pass on the host frame (fbo, w x h), before its first sprite: the scene depth copied and bound,
// the hardware's test made to pass always (the shader tests against the copy). False: no copy - the frame is drawn with
// the hardware's test and leaves no depth.
func (r *Renderer_GL33) ovDepthBegin(fbo uint32, w, h int32, depthLEqual bool) bool {
	d := &ovDepth
	if d.failed || w <= 0 || h <= 0 {
		return false
	}
	ovGLDirect()
	gl.ActiveTexture(gl.TEXTURE0)
	bound := false // (a texture or renderbuffer bound below: the frame's caches are forgotten after)
	d.srcAge++
	if fbo != d.src || w != d.srcW || h != d.srcH || d.srcAge >= ovDepthReread {
		gl.BindFramebuffer(gl.FRAMEBUFFER, fbo)
		typ, _, format, samples := ovAttachment(gl.DEPTH_ATTACHMENT)
		bound = true
		d.src, d.srcW, d.srcH, d.srcAge, d.srcFormat = fbo, w, h, 0, format
		if typ == 0 || samples > 1 {
			d.srcFormat = 0
			d.say(fmt.Sprintf("no single-sample depth on the frame (%d x%d): no depth left for the host", typ, samples))
		}
	}
	format := d.srcFormat
	if format == 0 {
		if bound {
			r.ovInvalidateStateCache()
		}
		return false
	}
	if d.fb == 0 || d.w != w || d.h != h || d.format != format {
		bound = true
		d.free()
		gl.GenTextures(1, &d.tex)
		gl.BindTexture(gl.TEXTURE_2D, d.tex)
		gl.TexParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.NEAREST)
		gl.TexParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.NEAREST)
		gl.TexParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_S, gl.CLAMP_TO_EDGE)
		gl.TexParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_T, gl.CLAMP_TO_EDGE)
		gl.TexParameteri(gl.TEXTURE_2D, gl.TEXTURE_COMPARE_MODE, gl.NONE)
		upload, xtype := uint32(gl.DEPTH_STENCIL), uint32(gl.UNSIGNED_INT_24_8)
		attach := uint32(gl.DEPTH_STENCIL_ATTACHMENT)
		switch uint32(format) {
		case gl.DEPTH24_STENCIL8:
		case gl.DEPTH32F_STENCIL8:
			xtype = gl.FLOAT_32_UNSIGNED_INT_24_8_REV
		case gl.DEPTH_COMPONENT24, gl.DEPTH_COMPONENT16, gl.DEPTH_COMPONENT32:
			upload, xtype, attach = gl.DEPTH_COMPONENT, gl.UNSIGNED_INT, gl.DEPTH_ATTACHMENT
		case gl.DEPTH_COMPONENT32F:
			upload, xtype, attach = gl.DEPTH_COMPONENT, gl.FLOAT, gl.DEPTH_ATTACHMENT
		default:
			d.free()
			d.srcFormat = 0
			d.say(fmt.Sprintf("frame depth format 0x%x unknown: no depth left for the host", format))
			gl.BindFramebuffer(gl.FRAMEBUFFER, fbo)
			r.ovInvalidateStateCache()
			return false
		}
		gl.TexImage2D(gl.TEXTURE_2D, 0, format, w, h, 0, upload, xtype, nil)
		gl.GenFramebuffers(1, &d.fb)
		gl.BindFramebuffer(gl.FRAMEBUFFER, d.fb)
		gl.FramebufferTexture2D(gl.FRAMEBUFFER, attach, gl.TEXTURE_2D, d.tex, 0)
		gl.DrawBuffer(gl.NONE)
		gl.ReadBuffer(gl.NONE)
		if gl.CheckFramebufferStatus(gl.FRAMEBUFFER) != gl.FRAMEBUFFER_COMPLETE {
			d.free()
			d.failed = true
			d.say("scene depth copy incomplete: no depth left for the host")
			gl.BindFramebuffer(gl.FRAMEBUFFER, fbo)
			r.ovInvalidateStateCache()
			return false
		}
		d.w, d.h, d.format = w, h, format
		gl.BindTexture(gl.TEXTURE_2D, 0)
	}
	// the scene depth as it is before any sprite
	gl.BindFramebuffer(gl.READ_FRAMEBUFFER, fbo)
	gl.BindFramebuffer(gl.DRAW_FRAMEBUFFER, d.fb)
	gl.BlitFramebuffer(0, 0, w, h, 0, 0, w, h, gl.DEPTH_BUFFER_BIT, gl.NEAREST)
	gl.BindFramebuffer(gl.FRAMEBUFFER, fbo)
	gl.ActiveTexture(gl.TEXTURE0 + ovWorldDepthUnit)
	gl.BindTexture(gl.TEXTURE_2D, d.tex)
	gl.ActiveTexture(gl.TEXTURE0)
	// (the frame was begun before - ovBeginFrame -, and only bindings outside the caches changed since, unless bound)
	if bound {
		r.ovInvalidateStateCache()
		d.say(fmt.Sprintf("Ikemen's frame under the host's transparent objects, its solid sprites' depth left for them (%dx%d, depth 0x%x)",
			w, h, format))
	}
	r.ov.activeUnit = 0
	ovLate = true
	ovLateTest = 1
	if depthLEqual {
		ovLateTest = -1
	}
	r.ovLateDepthState()
	return true
}

// ovDepthEnd: the late pass over - no depth is written after it
func (r *Renderer_GL33) ovDepthEnd(depthLEqual bool) {
	if !ovLate {
		return
	}
	ovGLDirect()
	ovLate = false
	gl.DepthMask(false)
	r.depthMask = false
	if depthLEqual {
		gl.DepthFunc(gl.LEQUAL)
	} else {
		gl.DepthFunc(gl.GEQUAL)
	}
}

func (d *ovDepthGL) free() {
	if d.fb != 0 {
		gl.DeleteFramebuffers(1, &d.fb)
	}
	if d.tex != 0 {
		gl.DeleteTextures(1, &d.tex)
	}
	d.fb, d.tex, d.w, d.h, d.format = 0, 0, 0, 0, 0
}

// ovLateDepthState: ovBeginFrame's depth state during the late pass - the test on (depth is only written with it on),
// passing always (the sprite shader tests against the scene copy), nothing written until a sprite's opaque pass
func (r *Renderer_GL33) ovLateDepthState() {
	gl.Enable(gl.DEPTH_TEST)
	gl.DepthFunc(gl.ALWAYS)
	gl.DepthMask(false)
	r.depthTest, r.depthMask = true, false
	r.ovHwTest = false
}

// ovSetDepthMask: depth writes on or off, through the fast path's queue while it queues
func (r *Renderer_GL33) ovSetDepthMask(on bool) {
	if r.depthMask == on {
		return
	}
	r.depthMask = on
	ovStateGen++
	if ovQueueing {
		ovOp2(ovOpDepthMask, uint32(Btoi(on)))
		return
	}
	ovGLDirect()
	gl.DepthMask(on)
}

// ovDepthSprite: a sprite's set-up for the late pass (both sprite paths, before its blend passes): Ikemen's sprite shader
// tests against the scene copy; a sprite with a shader of its own has no such test - the hardware's is made for it
// (against what is drawn so far), and it leaves no depth
func (r *Renderer_GL33) ovDepthSprite() {
	if r.spriteShader == nil {
		return
	}
	own := r.program == r.spriteShader.program
	if ovLate {
		r.ovHardwareTest(!own)
	}
	if !own {
		return
	}
	o := &r.ov // (the sprite shader's locations: ovInitFast)
	test := int32(0)
	if ovLate && !ovCard.screen { // (a screen-space sprite lies over the scene: no test, the hardware's passes)
		test = ovLateTest
	}
	r.SetUniformISub(o.locDepthTest, test)
	if test != 0 { // (ovDepthCut: set by every pass - ovDepthPass - and by a fill - ovDepthFill)
		r.SetUniformISub(o.locWorldDepth, ovWorldDepthUnit)
		if o.locUpright >= 0 {
			p := &ovCard.upright
			r.SetUniformFSub(o.locUpright, p[0], p[1], p[2], p[3])
		}
		if o.locUprightAt >= 0 {
			p := &ovCard.uprightAt
			if !ovUprightTestOn {
				p = &[4]float32{} // (dev: the card, as before)
			}
			r.SetUniformFSub(o.locUprightAt, p[0], p[1], p[2], p[3])
		}
	}
}

// ovHardwareTest: during the late pass, the hardware's depth test made (on) or passing always (off)
func (r *Renderer_GL33) ovHardwareTest(on bool) {
	if r.ovHwTest == on {
		return
	}
	r.ovHwTest = on
	ovGLDirect()
	switch {
	case !on:
		gl.DepthFunc(gl.ALWAYS)
	case ovLateTest < 0:
		gl.DepthFunc(gl.LEQUAL)
	default:
		gl.DepthFunc(gl.GEQUAL)
	}
}

// The depth a sprite leaves is not its card's: Ikemen's cards face the camera - leaning back with its pitch, so a
// fighter's chest lies further back than its feet - while a host's effects usually stand upright. Compared card to
// upright effect, a flame a little behind a fighter's feet would stand in front of its chest. So a sprite leaves the
// depth of an upright billboard at its anchor, moved ovLateBias (world units; env IKEMEN_LATE_BIAS) toward the camera:
// an effect at the very spot of a fighter is drawn over it, one behind a fighter is hidden by it at every height.
var ovLateBias = float32(ovEnvF("IKEMEN_LATE_BIAS", 0.16))

// Dev (env IKEMEN_UPRIGHT_TEST=0): sprites tested against the host's scene with their card (setUpright)
var ovUprightTestOn = os.Getenv("IKEMEN_UPRIGHT_TEST") != "0"

// setUpright: the window-space depth planes (A, B, C; w 1) of the upright billboard through the card's anchor (z, ax:
// as setSprite's) - uprightAt at the anchor itself, the depth it is tested against the host's scene with (a prop in
// front of a fighter's feet hides it, one behind is hidden by it at every height), and upright moved ovLateBias toward
// the camera, the depth it leaves for the host's effects. The planes are the ones at that distance from the card
// origin toward the camera (n.P = n.A, n = (-rz, 0, rx)): every sprite at one distance - with the camera's right along
// x, every one at one depth, whatever its anchor's x - keeps the planes worked out first, so their uniforms are not
// uploaded again for each.
func (c *ovCardState) setUpright(z, ax float32) {
	if c.viewW <= 0 || c.viewH <= 0 {
		c.upright, c.uprightAt, c.uprightSet = [4]float32{}, [4]float32{}, false
		return
	}
	m := c.m
	y0 := c.worldY0
	if !ovCfg.ParityMode {
		y0 = c.worldY0 - c.ay*m // (the height its anchor really stands at)
	}
	key := m*(c.rx*z-c.rz*ax) + ovLateBias + 8191*y0
	if c.uprightSet && key == c.uprightKey {
		return
	}
	c.uprightKey, c.uprightSet = key, true
	// A = (X0 + ax*m, y0, Z0 + z*m) (setSprite), toward the camera: -Back = (-rz, 0, rx)
	ax0, az0 := c.worldX0+ax*m, c.worldZ0+z*m
	c.uprightAt = c.uprightPlane(ax0, y0, az0)
	c.upright = c.uprightPlane(ax0-c.rz*ovLateBias, y0, az0+c.rx*ovLateBias)
}

// uprightPlane: the window-space depth plane of the upright billboard standing at world point (x, y, z), facing the
// card's right; w 0 when the camera cannot place it
func (c *ovCardState) uprightPlane(x, y, z float32) [4]float32 {
	var win [3][3]float64
	for i, d := range [3][3]float32{{0, 0, 0}, {c.rx, 0, c.rz}, {0, 1, 0}} { // (the point, a unit right, a unit up)
		q := ovVP64(&c.vp, float64(x+d[0]), float64(y+d[1]), float64(z+d[2])) // (double precision: host_ov_frame.go ovVP64)
		if q[3] < 1e-4 {
			return [4]float32{}
		}
		iw := 1 / q[3]
		win[i] = [3]float64{
			(q[0]*iw*0.5 + 0.5) * float64(c.viewW),
			(q[1]*iw*0.5 + 0.5) * float64(c.viewH),
			float64(c.depthRange[0]) + float64(c.depthRange[1]-c.depthRange[0])*(q[2]*iw*0.5+0.5),
		}
	}
	d1 := [3]float64{win[1][0] - win[0][0], win[1][1] - win[0][1], win[1][2] - win[0][2]}
	d2 := [3]float64{win[2][0] - win[0][0], win[2][1] - win[0][1], win[2][2] - win[0][2]}
	nx := d1[1]*d2[2] - d1[2]*d2[1]
	ny := d1[2]*d2[0] - d1[0]*d2[2]
	nz := d1[0]*d2[1] - d1[1]*d2[0]
	if math.Abs(nz) < 1e-6 {
		return [4]float32{}
	}
	a, b := -nx/nz, -ny/nz
	return [4]float32{float32(a), float32(b), float32(win[0][2] - a*win[0][0] - b*win[0][1]), 1}
}

// ovDepthPass: one blend pass of a sprite in the late pass (ovRenderBlend): its opaque pass - one plain "over" at full
// alpha - writes the depth of its solid texels; every other (added, subtracted, translucent, darkening) writes none
func (r *Renderer_GL33) ovDepthPass(eq BlendEquation, src, dst BlendFunc, a float32) {
	own := r.spriteShader != nil && r.program == r.spriteShader.program
	solid := own && ovLateDepthOn && ovCard.upright[3] != 0 && eq == BlendAdd && dst == BlendOneMinusSrcAlpha && a >= 1 &&
		(src == BlendSrcAlpha || src == BlendOne)
	if own {
		r.SetUniformISub(r.ov.locDepthCut, int32(Btoi(solid)*(1+Btoi(ovLateDepthDebug))))
	}
	r.ovSetDepthMask(solid)
}

// ovDepthFill: a fill (fillRectState, Ikemen's sprite shader bound) in the late pass - it leaves no depth and cuts none
// of its pixels; its test against the scene is the sprite before it's
func (r *Renderer_GL33) ovDepthFill() {
	if !ovLate || r.spriteShader == nil || r.program != r.spriteShader.program {
		return
	}
	r.SetUniformISub(r.ov.locDepthCut, 0)
	r.ovSetDepthMask(false)
}

// ---------------------------------------------------------------------------------------------------------------------
// The draw order in a 3D scene (DrawList.draw, every layer in one pass: System.draw). Ikemen sorts each layer apart,
// by priority = sprpriority + the sprite's z in game units, so a large sprpriority draws a fighter over one standing
// well in front of it, and an effect on top (layer 1) over a fighter standing in front of it. With fighters spread
// over a floor's depth and a camera that sees them in 3D the order is the 3D one, back to front:
//   - depth, farther first: a sprite at its fighter's depth, or within ovDepthTie of any fighter's, takes that depth -
//     a fighter and its own effects, a thrower and what it holds stand at one;
//   - then MUGEN's layers (behind, under, the fighters', on top), sprpriority, Ikemen's newer-first;
//   - screen-space sprites over all of it, in MUGEN's own order.
// The parity profile keeps Ikemen's order and passes; env IKEMEN_DEPTH_ORDER=0 keeps them too.

var ovDepthOrderOn = os.Getenv("IKEMEN_DEPTH_ORDER") != "0"

// ovAllLayers: DrawList.draw's layer number for one pass over every layer (the depth order)
const ovAllLayers = int32(math.MinInt32)

// ovDepthTie: depths closer than this (ik units) are one - float noise between a fighter and what it binds
const ovDepthTie = 0.5

// ovDepthOrderActive: DrawList.draw sorts by depth (the overlay's own drawing, never the parity profile)
func ovDepthOrderActive() bool {
	return ovDepthOrderOn && !ovCfg.ParityMode && ovDrawActive()
}

var ovDepthRoots []float32
var ovDepthChains [][2]float32 // (one pass's fighter depths chained within ovDepthTie: the first and last of each)

// One sprite's sort key in one uint64, high bits first: screen-space (1 bit), depth (22 bits: 1/256 ik unit, +-8192
// units), layer (2 bits), priority (19 bits, +-2^18), the later listed first (18 bits: Ikemen's sync-group /
// newer-first order of a pass without sync groups). Unique, so the order is exact without a stable sort.
const (
	ovKeyPos   = 18
	ovKeyPrio  = 19 // (+-2^18: sprpriority and Ikemen's z part stay far inside)
	ovKeyDepth = 22
)

var ovDepthKeyBuf []uint64
var ovDepthListBuf []int
var ovDepthKeyScratch []uint64

// ovRadixOn: the depth keys sorted by radix passes (env IKEMEN_RADIX=0: slices.Sort - the same order)
var ovRadixOn = os.Getenv("IKEMEN_RADIX") != "0"

func ovKeyField(v int64, bits uint) uint64 {
	half := int64(1) << (bits - 1)
	if v < -half {
		v = -half
	} else if v > half-1 {
		v = half - 1
	}
	return uint64(v + half)
}

// ovDepthSort: one pass's sprites (list: their indices in dl, sync groups assigned) sorted back to front - true; false
// when the pass has a sync group (rare: DrawList.draw sorts it by the keys set here) or more sprites than a key numbers
func ovDepthSort(dl DrawList, list []int) bool {
	if !ovDepthKeys(dl, list) || len(list) >= 1<<ovKeyPos {
		return false
	}
	keys := ovDepthKeyBuf[:0]
	for k, i := range list {
		s := dl[i]
		key := ovKeyField(int64(math.Floor(float64(s.ovSortZ)*256+0.5)), ovKeyDepth)
		key = key<<2 | uint64(s.ovSortLayer)
		key = key<<ovKeyPrio | ovKeyField(int64(s.priority), ovKeyPrio)
		key = key<<ovKeyPos | uint64(1<<ovKeyPos-1-k)
		if s.ovSortRank != 0 { // (a screen-space sprite: over everything in the scene)
			key |= 1 << 63
		}
		keys = append(keys, key)
	}
	if ovRadixOn { // (the same ascending order, by radix passes - host_ov_radix.go)
		ovDepthKeyScratch = ovRadixSort64(keys, ovDepthKeyScratch)
	} else {
		slices.Sort(keys)
	}
	was := append(ovDepthListBuf[:0], list...)
	for n, key := range keys {
		list[n] = was[1<<ovKeyPos-1-int(key&(1<<ovKeyPos-1))]
	}
	ovDepthKeyBuf, ovDepthListBuf = keys, was
	ovDepthOrderLog(dl, list)
	return true
}

// Dev: env IKEMEN_ORDER_LOG=<tick>[,<tick>...] - the pass's order at those overlay ticks (overlay.log "depth order")
var ovOrderLogTicks = func() map[int32]bool {
	m := map[int32]bool{}
	for _, f := range strings.Split(os.Getenv("IKEMEN_ORDER_LOG"), ",") {
		if v, err := strconv.Atoi(strings.TrimSpace(f)); err == nil {
			m[int32(v)] = true
		}
	}
	return m
}()

func ovDepthOrderLog(dl DrawList, list []int) {
	if len(ovOrderLogTicks) == 0 || !ovOrderLogTicks[ov.tick] || !ovCapturing() {
		return
	}
	delete(ovOrderLogTicks, ov.tick)
	LogMessage("[overlay] depth order at tick %d: %d sprites, back to front", ov.tick, len(list))
	for n, i := range list {
		s := dl[i]
		who, kind := "-", map[int8]string{0: "body", 1: "helper", 2: "explod", 3: "projectile"}[s.ovKind]
		if s.ovRoot != nil {
			who = fmt.Sprintf("%s(p%d)", s.ovRoot.name, s.ovRoot.playerNo)
		}
		LogMessage("[overlay]   %3d %-22s %-10s anim %6d x %7.1f z %7.1f key %7.1f screen %d layer %d prio %6d trans %d", n, who,
			kind, s.ovAnimNo, s.ovX, s.ovZ, s.ovSortZ, s.ovSortRank, s.ovSortLayer, s.priority, s.trans)
	}
}

// ovDepthKeys: the sort keys of one pass's sprites (ovSortZ, ovSortRank, ovSortLayer). The depths of the sprites' root
// fighters chain within ovDepthTie; a sprite at its root's depth, or at any chain's, takes that chain's first depth, any
// other its own. A sync group's members take their leader's key: they stay together. False: the pass has a sync group.
func ovDepthKeys(dl DrawList, list []int) bool {
	roots := ovDepthRoots[:0]
	for _, i := range list {
		// (NaN: no root; a fighter's sprites are mostly listed together - its depth once for them)
		if z := dl[i].ovRootZ; z == z && (len(roots) == 0 || roots[len(roots)-1] != z) {
			roots = append(roots, z)
		}
	}
	slices.Sort(roots)
	chains := ovDepthChains[:0]
	for _, z := range roots {
		if n := len(chains); n > 0 && z-chains[n-1][1] <= ovDepthTie {
			chains[n-1][1] = z
		} else {
			chains = append(chains, [2]float32{z, z})
		}
	}
	ovDepthRoots, ovDepthChains = roots, chains
	plain := true
	for k, i := range list {
		s := dl[i]
		if s.syncId > 0 {
			plain = false
		}
		if s.syncGroup != k { // (a sync group's member: its leader comes first in list)
			lead := dl[list[s.syncGroup]]
			s.ovSortZ, s.ovSortRank, s.ovSortLayer = lead.ovSortZ, lead.ovSortRank, lead.ovSortLayer
			continue
		}
		z := s.ovZ
		if r := s.ovRootZ; r == r && z-r <= ovDepthTie && r-z <= ovDepthTie {
			z = r
		}
		// (the first chain reaching z: chains lie more than ovDepthTie apart)
		lo, hi := 0, len(chains)
		for lo < hi {
			if m := int(uint(lo+hi) >> 1); chains[m][1]+ovDepthTie < z {
				lo = m + 1
			} else {
				hi = m
			}
		}
		if lo < len(chains) && chains[lo][0]-ovDepthTie <= z {
			z = chains[lo][0]
		} else if z != z {
			z = 0
		}
		if z == 0 {
			z = 0 // (+0 for -0 too: one key)
		}
		s.ovSortZ, s.ovSortRank = z, uint8(Btoi(s.screen))
		switch {
		case s.layerno < 0:
			s.ovSortLayer = 0
		case s.layerno == 0 && s.under:
			s.ovSortLayer = 1
		case s.layerno == 0:
			s.ovSortLayer = 2
		default:
			s.ovSortLayer = 3
		}
	}
	return plain
}
