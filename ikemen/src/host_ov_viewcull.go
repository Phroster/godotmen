package main

import (
	"os"

	mgl "github.com/go-gl/mathgl/mgl32"
)

// Overlay: sprites off the host's view are not drawn. A wide floor keeps everything left on it - fighters, bodies,
// effects that stay - and the frozen renderer would draw every one of them every frame (uniforms, texture binds, draw
// calls) wherever the camera stands.
//
// A sprite is skipped when its own quad (not its owner's anchor: art can sit far from its axis) lies wholly past the
// left or the right side of the view, by more than ovViewCullMargin times the half view. Only plain quads are tested
// (no tiling, no rotation, no trapezoid); anything else is drawn as ever, as are screen-space sprites. Not in the
// parity profile. Env IKEMEN_VIEW_CULL=0: nothing is skipped; IKEMEN_VIEW_CULL_MARGIN: the margin (default 1.5).
var ovViewCullOn = os.Getenv("IKEMEN_VIEW_CULL") != "0"
var ovViewCullMargin = float32(ovEnvF("IKEMEN_VIEW_CULL_MARGIN", 1.5))

var ovViewCull struct {
	on bool     // a frozen frame is being drawn with the host's camera in vp
	vp mgl.Mat4 // the host's view-projection (ovCard.vp as the frame began)
}

var ovFrameCulled int // this overlay frame: sprites skipped
var ovStatCulled int64

// ovViewCullBegin: (ovDrawFrozen, the card set up) this frame's camera
func ovViewCullBegin() {
	ovFrameCulled = 0
	ovViewCull.on = ovViewCullOn && !ovCfg.ParityMode && ovViewCullMargin >= 1
	ovViewCull.vp = ovCard.vp
}

func ovViewCullEnd() { ovViewCull.on = false }

// ovSpriteOffView: (ovRenderSprite, the card's basis current) rp's quad lies wholly past one side of the view
func ovSpriteOffView(rp *RenderParams, screenH float32) bool {
	v := &ovViewCull
	if !v.on || ovCard.screen || rp.tile.xflag != 0 || rp.tile.yflag != 0 || !rp.rot.IsZero() || rp.xts != rp.xbs ||
		rp.projectionMode != 0 {
		return false
	}
	// (renderSpriteQuad's corners, through its modelview - a shift down by the screen's height)
	w, h := float32(rp.size[0]), float32(rp.size[1])
	xb := rp.x + rp.rxadd*rp.ys*h
	yb := rp.rcy + ((rp.y-rp.ys*h)-rp.rcy)*rp.vs + screenH
	yt := rp.rcy + (rp.y-rp.rcy)*rp.vs + screenH
	quad := [4][2]float32{{xb, yb}, {xb + rp.xbs*w, yb}, {rp.x + rp.xts*w, yt}, {rp.x, yt}}
	b, vp, k := &ovCard.basis, &v.vp, ovViewCullMargin
	left, right := 0, 0
	for _, p := range quad {
		wx := b[0]*p[0] + b[4]*p[1] + b[12]
		wy := b[1]*p[0] + b[5]*p[1] + b[13]
		wz := b[2]*p[0] + b[6]*p[1] + b[14]
		cx := vp[0]*wx + vp[4]*wy + vp[8]*wz + vp[12]
		cw := vp[3]*wx + vp[7]*wy + vp[11]*wz + vp[15]
		switch {
		case !(cw > 1e-4): // (behind the camera, or not a number: drawn)
			return false
		case cx < -k*cw:
			left++
		case cx > k*cw:
			right++
		default:
			return false
		}
	}
	return left == 4 || right == 4
}
