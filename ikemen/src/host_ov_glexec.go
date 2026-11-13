//go:build overlay

package main

// Overlay: the fast sprite path's GL command queue executor (host_ov_render.go). The queued calls are
// the same GL calls with the same arguments in the same order, made from C in one cgo call instead of one cgo call
// each; consecutive draws with no state change in between (the same textures, uniforms and blend) go to the driver as
// one glMultiDrawArrays, which GL defines as those glDrawArrays one after another.

/*
#include <windows.h>
#include <stdint.h>
#include <string.h>

typedef void (APIENTRY *mk_u1i_t)(int, int);
typedef void (APIENTRY *mk_u1f_t)(int, float);
typedef void (APIENTRY *mk_u2f_t)(int, float, float);
typedef void (APIENTRY *mk_u3f_t)(int, float, float, float);
typedef void (APIENTRY *mk_u4f_t)(int, float, float, float, float);
typedef void (APIENTRY *mk_um4_t)(int, int, unsigned char, const float *);
typedef void (APIENTRY *mk_e1_t)(unsigned);
typedef void (APIENTRY *mk_e2_t)(unsigned, unsigned);
typedef void (APIENTRY *mk_draw_t)(unsigned, int, int);
typedef void (APIENTRY *mk_mdraw_t)(unsigned, const int *, const int *, int);
typedef void (APIENTRY *mk_b1_t)(unsigned char);

static mk_u1i_t mk_Uniform1i;
static mk_u1f_t mk_Uniform1f;
static mk_u2f_t mk_Uniform2f;
static mk_u3f_t mk_Uniform3f;
static mk_u4f_t mk_Uniform4f;
static mk_um4_t mk_UniformMatrix4fv;
static mk_e1_t mk_ActiveTexture, mk_Enable, mk_BlendEquation, mk_BindVertexArray;
static mk_e2_t mk_BindTexture, mk_BlendFunc;
static mk_draw_t mk_DrawArrays;
static mk_mdraw_t mk_MultiDrawArrays;
static mk_b1_t mk_DepthMask;

// (the same lookup go-gl does: the ICD's entry point, else opengl32's own export for GL 1.1 functions)
static void *mk_proc(const char *name) {
	void *p = (void *)wglGetProcAddress(name);
	if (p == 0 || p == (void *)1 || p == (void *)2 || p == (void *)3 || p == (void *)-1) {
		HMODULE m = GetModuleHandleA("opengl32.dll");
		p = m ? (void *)GetProcAddress(m, name) : 0;
	}
	return p;
}

// Needs the host's GL context current (ikemen_gl_init); 1 when every entry point was found
static int ikemen_glexec_init(void) {
	mk_Uniform1i = (mk_u1i_t)mk_proc("glUniform1i");
	mk_Uniform1f = (mk_u1f_t)mk_proc("glUniform1f");
	mk_Uniform2f = (mk_u2f_t)mk_proc("glUniform2f");
	mk_Uniform3f = (mk_u3f_t)mk_proc("glUniform3f");
	mk_Uniform4f = (mk_u4f_t)mk_proc("glUniform4f");
	mk_UniformMatrix4fv = (mk_um4_t)mk_proc("glUniformMatrix4fv");
	mk_ActiveTexture = (mk_e1_t)mk_proc("glActiveTexture");
	mk_Enable = (mk_e1_t)mk_proc("glEnable");
	mk_BlendEquation = (mk_e1_t)mk_proc("glBlendEquation");
	mk_BindVertexArray = (mk_e1_t)mk_proc("glBindVertexArray");
	mk_BindTexture = (mk_e2_t)mk_proc("glBindTexture");
	mk_BlendFunc = (mk_e2_t)mk_proc("glBlendFunc");
	mk_DrawArrays = (mk_draw_t)mk_proc("glDrawArrays");
	mk_MultiDrawArrays = (mk_mdraw_t)mk_proc("glMultiDrawArrays");
	mk_DepthMask = (mk_b1_t)mk_proc("glDepthMask");
	return mk_Uniform1i && mk_Uniform1f && mk_Uniform2f && mk_Uniform3f && mk_Uniform4f && mk_UniformMatrix4fv &&
		mk_ActiveTexture && mk_Enable && mk_BlendEquation && mk_BindVertexArray && mk_BindTexture && mk_BlendFunc &&
		mk_DrawArrays && mk_MultiDrawArrays && mk_DepthMask;
}

static float mk_f(uint32_t bits) {
	float f;
	memcpy(&f, &bits, 4);
	return f;
}

// Op codes: keep in step with host_ov_render.go (ovOp*)
enum { MK_U1I = 1, MK_U1F, MK_U2F, MK_U3F, MK_U4F, MK_UM4, MK_ACTIVE, MK_BINDTEX, MK_ENABLE, MK_BLENDEQ, MK_BLENDFUNC,
	MK_BINDVAO, MK_DRAW, MK_DEPTHMASK };

#define MK_TRIANGLE_STRIP 0x0005
#define MK_TEXTURE_2D 0x0DE1
#define MK_MAXMULTI 1024

// Runs n words of queued GL calls; returns the GL draw calls made, or -1 - (word index) on a bad op
static int ikemen_glexec(const uint32_t *o, int n) {
	static int firsts[MK_MAXMULTI], counts[MK_MAXMULTI];
	int i = 0, calls = 0;
	while (i < n) {
		switch (o[i]) {
		case MK_U1I: mk_Uniform1i((int)o[i + 1], (int)o[i + 2]); i += 3; break;
		case MK_U1F: mk_Uniform1f((int)o[i + 1], mk_f(o[i + 2])); i += 3; break;
		case MK_U2F: mk_Uniform2f((int)o[i + 1], mk_f(o[i + 2]), mk_f(o[i + 3])); i += 4; break;
		case MK_U3F: mk_Uniform3f((int)o[i + 1], mk_f(o[i + 2]), mk_f(o[i + 3]), mk_f(o[i + 4])); i += 5; break;
		case MK_U4F:
			mk_Uniform4f((int)o[i + 1], mk_f(o[i + 2]), mk_f(o[i + 3]), mk_f(o[i + 4]), mk_f(o[i + 5]));
			i += 6;
			break;
		case MK_UM4: mk_UniformMatrix4fv((int)o[i + 1], 1, 0, (const float *)(o + i + 2)); i += 18; break;
		case MK_ACTIVE: mk_ActiveTexture(o[i + 1]); i += 2; break;
		case MK_BINDTEX: mk_BindTexture(MK_TEXTURE_2D, o[i + 1]); i += 2; break;
		case MK_ENABLE: mk_Enable(o[i + 1]); i += 2; break;
		case MK_BLENDEQ: mk_BlendEquation(o[i + 1]); i += 2; break;
		case MK_BLENDFUNC: mk_BlendFunc(o[i + 1], o[i + 2]); i += 3; break;
		case MK_BINDVAO: mk_BindVertexArray(o[i + 1]); i += 2; break;
		case MK_DEPTHMASK: mk_DepthMask((unsigned char)o[i + 1]); i += 2; break;
		case MK_DRAW: {
			int k = 0;
			while (i < n && o[i] == MK_DRAW && k < MK_MAXMULTI) {
				firsts[k] = (int)o[i + 1];
				counts[k] = 4;
				k++;
				i += 2;
			}
			if (k == 1) {
				mk_DrawArrays(MK_TRIANGLE_STRIP, firsts[0], 4);
			} else {
				mk_MultiDrawArrays(MK_TRIANGLE_STRIP, firsts, counts, k);
			}
			calls++;
			break;
		}
		default:
			return -1 - i;
		}
	}
	return calls;
}
*/
import "C"

import "unsafe"

// Loads the executor's GL entry points (GL thread, context current)
func ovGLExecInit() bool {
	return C.ikemen_glexec_init() != 0
}

// Runs n queued words (ovOps, a pointer-free global); returns the GL draw calls made
func ovGLExec(ops *uint32, n int) int {
	return int(C.ikemen_glexec((*C.uint32_t)(unsafe.Pointer(ops)), C.int(n)))
}
