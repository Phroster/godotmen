package main

// Overlay: small rendering helpers of the host draw that every sprite goes through.

import (
	"os"
	"strconv"

	gl "github.com/go-gl/gl/v3.3-core/gl"
	mgl "github.com/go-gl/mathgl/mgl32"
)

// ovEnvF: a float from the environment, def when it is unset or no number
func ovEnvF(name string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(name), 64); err == nil {
		return v
	}
	return def
}

// ovSetProjection: the sprite shader's projection for the quads that follow (ovRenderSprite's, or the legacy path's)
func ovSetProjection(m *mgl.Mat4) {
	if r := ovFastQuad; r != nil {
		r.ovUniMat4(r.ov.locProjection, (*[16]float32)(m))
		return
	}
	gfx.SetUniformMatrix("projection", m[:])
}

// ovHostVP: the host's P x V as its 3D pass uses it (y flipped under FLIP_Y, clip z negated for its reverse-Z depth
// under DEPTH_TEST unless params[6] says the depth is standard)
func ovHostVP(vp, params [16]float32, flags int) mgl.Mat4 {
	fy, fz := float32(1), float32(1)
	if flags&ovFlagFlipY != 0 {
		fy = -1
	}
	if flags&ovFlagDepthTest != 0 && params[6] != 1 {
		fz = -1
	}
	m := mgl.Mat4(vp)
	if fy != 1 || fz != 1 {
		m = mgl.Scale3D(1, fy, fz).Mul4(m)
	}
	return m
}

// ovRenderBlend: a sprite's blend passes (renderWithBlending); in the late pass an opaque pass leaves its depth
// (host_ov_depth.go)
func ovRenderBlend(render func(eq BlendEquation, src, dst BlendFunc, a float32), rp *RenderParams, spfx *ShaderPalFX) {
	if r, ok := gfx.(*Renderer_GL33); ok && ovLate {
		inner := render
		render = func(eq BlendEquation, src, dst BlendFunc, a float32) {
			r.ovDepthPass(eq, src, dst, a)
			inner(eq, src, dst, a)
		}
	}
	renderWithBlending(render, rp.blendMode, rp.blendAlpha, rp.paltex != nil, spfx, rp.paltex == nil)
}

// ovAttachment: the object at an attachment point of the framebuffer bound to GL_FRAMEBUFFER - its type
// (GL_RENDERBUFFER, GL_TEXTURE; 0 none), name, internal format and samples (a texture's: level 0 of a 2D one, read
// through texture unit 0 - the caller made it active)
func ovAttachment(point uint32) (typ int32, name uint32, format, samples int32) {
	gl.GetFramebufferAttachmentParameteriv(gl.FRAMEBUFFER, point, gl.FRAMEBUFFER_ATTACHMENT_OBJECT_TYPE, &typ)
	if typ != gl.RENDERBUFFER && typ != gl.TEXTURE {
		return 0, 0, 0, 0
	}
	var n int32
	gl.GetFramebufferAttachmentParameteriv(gl.FRAMEBUFFER, point, gl.FRAMEBUFFER_ATTACHMENT_OBJECT_NAME, &n)
	name = uint32(n)
	if typ == gl.RENDERBUFFER {
		gl.BindRenderbuffer(gl.RENDERBUFFER, name)
		gl.GetRenderbufferParameteriv(gl.RENDERBUFFER, gl.RENDERBUFFER_INTERNAL_FORMAT, &format)
		gl.GetRenderbufferParameteriv(gl.RENDERBUFFER, gl.RENDERBUFFER_SAMPLES, &samples)
		return
	}
	gl.BindTexture(gl.TEXTURE_2D, name)
	gl.GetTexLevelParameteriv(gl.TEXTURE_2D, 0, gl.TEXTURE_INTERNAL_FORMAT, &format)
	return
}
