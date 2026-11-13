// Test stand-in for ikemen.dll: exports the C ABI the module binds (ikemen_runtime.cpp) and, in
// ikemen_render, draws two triangles with raw GL into the FBO Godot passes, then deliberately leaves a lot of GL
// state dirty so the module's IkemenGLStateGuard is exercised.
//   red triangle   : no depth test, drawn over the 3D scene and under the HUD canvas layers
//   green triangle : DEPTH_TEST flag -> GL_GEQUAL against the scene depth, at the depth of world point
//                    (0, 0, -3) projected with view_proj (behind the test cube: hidden by it, visible over the
//                    background)
//   blue triangle  : DEPTH_TEST, centred on world point (0, 0, 2.5) projected with view_proj (in front of the
//                    cube: drawn over it) - checks set_camera()'s column-major matrix end to end
// Build (MSYS2 clang64):
//   clang -shared -O2 -o test.dll test_dll.c -lopengl32
// Point the module at it with the environment variable IKEMEN_DLL=<path to test.dll>.
#include <windows.h>

#include <GL/gl.h>
#include <stddef.h>
#include <stdio.h>
#include <string.h>

#define EXPORT __declspec(dllexport)

typedef char GLchar;
typedef ptrdiff_t GLsizeiptr;

#define GL_FRAMEBUFFER 0x8D40
#define GL_ARRAY_BUFFER 0x8892
#define GL_STATIC_DRAW 0x88E4
#define GL_VERTEX_SHADER 0x8B31
#define GL_FRAGMENT_SHADER 0x8B30
#define GL_COMPILE_STATUS 0x8B81
#define GL_LINK_STATUS 0x8B82
#define GL_MULTISAMPLE 0x809D
#define GL_TEXTURE0 0x84C0
#define GL_FUNC_ADD 0x8006
#define GL_FUNC_REVERSE_SUBTRACT 0x800B
#define GL_SHADING_LANGUAGE_VERSION 0x8B8C

#define GLFN(ret, name, args) \
	typedef ret(APIENTRY *PFN_##name) args; \
	static PFN_##name p_##name;
GLFN(GLuint, glCreateShader, (GLenum))
GLFN(void, glShaderSource, (GLuint, GLsizei, const GLchar *const *, const GLint *))
GLFN(void, glCompileShader, (GLuint))
GLFN(void, glGetShaderiv, (GLuint, GLenum, GLint *))
GLFN(void, glGetShaderInfoLog, (GLuint, GLsizei, GLsizei *, GLchar *))
GLFN(GLuint, glCreateProgram, (void))
GLFN(void, glAttachShader, (GLuint, GLuint))
GLFN(void, glBindAttribLocation, (GLuint, GLuint, const GLchar *))
GLFN(void, glLinkProgram, (GLuint))
GLFN(void, glGetProgramiv, (GLuint, GLenum, GLint *))
GLFN(void, glUseProgram, (GLuint))
GLFN(GLint, glGetUniformLocation, (GLuint, const GLchar *))
GLFN(void, glUniform4f, (GLint, GLfloat, GLfloat, GLfloat, GLfloat))
GLFN(void, glUniform2f, (GLint, GLfloat, GLfloat))
GLFN(void, glUniform1f, (GLint, GLfloat))
GLFN(void, glGenVertexArrays, (GLsizei, GLuint *))
GLFN(void, glBindVertexArray, (GLuint))
GLFN(void, glGenBuffers, (GLsizei, GLuint *))
GLFN(void, glBindBuffer, (GLenum, GLuint))
GLFN(void, glBufferData, (GLenum, GLsizeiptr, const void *, GLenum))
GLFN(void, glVertexAttribPointer, (GLuint, GLint, GLenum, GLboolean, GLsizei, const void *))
GLFN(void, glEnableVertexAttribArray, (GLuint))
GLFN(void, glBindFramebuffer, (GLenum, GLuint))
GLFN(void, glBlendEquation, (GLenum))
GLFN(void, glActiveTexture, (GLenum))

static char g_error[512] = "";
static char g_events[8192] = "";
static int g_status = 0;
static int g_ticks = 0;
static int g_renders = 0;
static int g_pumps = 0;
static int g_commands = 0;
static GLuint g_prog, g_vao, g_vbo, g_tex;
static GLint u_flip, u_z, u_offset, u_scale, u_color;

static void push_event(const char *line) {
	size_t used = strlen(g_events), add = strlen(line);
	if (used + add + 2 < sizeof(g_events)) {
		memcpy(g_events + used, line, add);
		g_events[used + add] = '\n';
		g_events[used + add + 1] = 0;
	}
}

static void *load_gl(const char *name) {
	void *p = (void *)wglGetProcAddress(name);
	if (p == 0 || p == (void *)1 || p == (void *)2 || p == (void *)3 || p == (void *)-1) {
		HMODULE gl = GetModuleHandleA("opengl32.dll");
		p = gl ? (void *)GetProcAddress(gl, name) : 0;
	}
	return p;
}

#define LOAD(name)                                                                    \
	p_##name = (PFN_##name)load_gl(#name);                                            \
	if (!p_##name) {                                                                  \
		snprintf(g_error, sizeof(g_error), "test dll: GL function %s missing", #name); \
		return -2;                                                                    \
	}

static const char *VS =
		"#version 330 core\n"
		"layout(location = 0) in vec2 pos;\n"
		"uniform float flip; uniform float z; uniform vec2 offset; uniform float scale;\n"
		"void main() { vec2 p = pos * scale + offset; gl_Position = vec4(p.x, p.y * flip, z, 1.0); }\n";
static const char *FS =
		"#version 330 core\n"
		"uniform vec4 color; out vec4 frag;\n"
		"void main() { frag = color; }\n";

static GLuint compile(GLenum type, const char *src) {
	GLuint s = p_glCreateShader(type);
	p_glShaderSource(s, 1, &src, 0);
	p_glCompileShader(s);
	GLint ok = 0;
	p_glGetShaderiv(s, GL_COMPILE_STATUS, &ok);
	if (!ok) {
		char log[400];
		p_glGetShaderInfoLog(s, sizeof(log), 0, log);
		snprintf(g_error, sizeof(g_error), "test dll: shader compile failed: %s", log);
		return 0;
	}
	return s;
}

EXPORT int ikemen_init(const char *root, const char *config_json) {
	char line[1024];
	snprintf(line, sizeof(line), "{\"e\":\"init\",\"root_len\":%d,\"config_len\":%d}", (int)strlen(root ? root : ""), (int)strlen(config_json ? config_json : ""));
	push_event(line);
	push_event("{\"e\":\"spawned\",\"h\":0}");
	g_status = 1;
	return 0;
}

EXPORT int ikemen_gl_init(void) {
	LOAD(glCreateShader) LOAD(glShaderSource) LOAD(glCompileShader) LOAD(glGetShaderiv) LOAD(glGetShaderInfoLog);
	LOAD(glCreateProgram) LOAD(glAttachShader) LOAD(glBindAttribLocation) LOAD(glLinkProgram) LOAD(glGetProgramiv);
	LOAD(glUseProgram) LOAD(glGetUniformLocation) LOAD(glUniform4f) LOAD(glUniform2f) LOAD(glUniform1f);
	LOAD(glGenVertexArrays) LOAD(glBindVertexArray) LOAD(glGenBuffers) LOAD(glBindBuffer) LOAD(glBufferData);
	LOAD(glVertexAttribPointer) LOAD(glEnableVertexAttribArray) LOAD(glBindFramebuffer) LOAD(glBlendEquation);
	LOAD(glActiveTexture);

	GLuint vs = compile(GL_VERTEX_SHADER, VS), fs = compile(GL_FRAGMENT_SHADER, FS);
	if (!vs || !fs) {
		return -3;
	}
	g_prog = p_glCreateProgram();
	p_glAttachShader(g_prog, vs);
	p_glAttachShader(g_prog, fs);
	p_glBindAttribLocation(g_prog, 0, "pos");
	p_glLinkProgram(g_prog);
	GLint ok = 0;
	p_glGetProgramiv(g_prog, GL_LINK_STATUS, &ok);
	if (!ok) {
		snprintf(g_error, sizeof(g_error), "test dll: program link failed");
		return -4;
	}
	u_flip = p_glGetUniformLocation(g_prog, "flip");
	u_z = p_glGetUniformLocation(g_prog, "z");
	u_offset = p_glGetUniformLocation(g_prog, "offset");
	u_scale = p_glGetUniformLocation(g_prog, "scale");
	u_color = p_glGetUniformLocation(g_prog, "color");

	// Triangle pointing UP in clip space (apex at +Y): proves the FLIP_Y convention in the saved screenshot.
	static const float tri[6] = { -1.0f, -1.0f, 1.0f, -1.0f, 0.0f, 1.0f };
	p_glGenVertexArrays(1, &g_vao);
	p_glBindVertexArray(g_vao);
	p_glGenBuffers(1, &g_vbo);
	p_glBindBuffer(GL_ARRAY_BUFFER, g_vbo);
	p_glBufferData(GL_ARRAY_BUFFER, sizeof(tri), tri, GL_STATIC_DRAW);
	p_glVertexAttribPointer(0, 2, GL_FLOAT, GL_FALSE, 0, 0);
	p_glEnableVertexAttribArray(0);
	glGenTextures(1, &g_tex);

	char line[600];
	snprintf(line, sizeof(line), "{\"e\":\"gl\",\"version\":\"%s\",\"renderer\":\"%s\"}", (const char *)glGetString(GL_VERSION), (const char *)glGetString(GL_RENDERER));
	push_event(line);
	return 0;
}

static int g_pending = 0; // (test) "texture uploads" queued by steps, drained by either pump
EXPORT void ikemen_gl_pump(void) {
	g_pumps++;
	g_pending = 0;
}

// Optional export: the module's idle pump (frames without the hook, e.g. a minimized window). Every step queues one
// "texture upload"; draining it dirties GL state like a real upload (unit 0 binding, UNPACK_ROW_LENGTH) so the test
// also proves the guard around the idle pump. usec < 0 only counts.
static int g_budget_pumps = 0;
EXPORT int ikemen_gl_pump_budget(int usec) {
	if (usec < 0) {
		return g_pending;
	}
	g_budget_pumps++;
	if (g_pending > 0) {
		p_glActiveTexture(GL_TEXTURE0);
		glBindTexture(GL_TEXTURE_2D, g_tex);
		glPixelStorei(GL_UNPACK_ROW_LENGTH, 7);
		g_pending = 0;
	}
	return 0;
}

EXPORT int ikemen_step(void) {
	g_ticks++;
	g_pending++;
	return 0;
}

static void draw_tri(float flip, float z, float ox, float oy, float scale, float r, float g, float b, float a) {
	p_glUniform1f(u_flip, flip);
	p_glUniform1f(u_z, z);
	p_glUniform2f(u_offset, ox, oy);
	p_glUniform1f(u_scale, scale);
	p_glUniform4f(u_color, r, g, b, a);
	glDrawArrays(GL_TRIANGLES, 0, 3);
}

// Clip-space projection of a world point with Godot's column-major view_proj: out = (ndc x, ndc y, ndc z).
static void project(const float *m, float wx, float wy, float wz, float *out) {
	float cx = m[0] * wx + m[4] * wy + m[8] * wz + m[12];
	float cy = m[1] * wx + m[5] * wy + m[9] * wz + m[13];
	float cz = m[2] * wx + m[6] * wy + m[10] * wz + m[14];
	float cw = m[3] * wx + m[7] * wy + m[11] * wz + m[15];
	if (cw == 0.0f) {
		cw = 1.0f;
	}
	out[0] = cx / cw;
	out[1] = cy / cw;
	out[2] = cz / cw;
}

static int g_logged_depth = 0;

EXPORT int ikemen_render(unsigned fbo, int w, int h, int flags, const float *view_proj16, const float *params16) {
	(void)params16;
	float behind[3], front[3];
	project(view_proj16, 0.0f, 0.0f, -3.0f, behind);
	project(view_proj16, 0.0f, 0.0f, 2.5f, front);
	// Godot's GLES3 scene pass draws with clip = diag(1, -1, -1, 1) * P * V (Projection::set_depth_correction(
	// flip_y, reverse_z = true, remap_z = false)): reverse-Z in OpenGL's [-1, 1] clip z. Match it.
	behind[2] = -behind[2];
	front[2] = -front[2];
	if (!g_logged_depth) {
		char line[256];
		snprintf(line, sizeof(line), "{\"e\":\"depth\",\"behind_ndc_z\":%.6f,\"front_ndc\":[%.4f,%.4f,%.6f]}", behind[2], front[0], front[1], front[2]);
		push_event(line);
		g_logged_depth = 1;
	}
	p_glBindFramebuffer(GL_FRAMEBUFFER, fbo);
	glViewport(0, 0, w, h);
	glDisable(GL_SCISSOR_TEST);
	glDisable(GL_CULL_FACE);
	glDisable(GL_STENCIL_TEST);
	glColorMask(GL_TRUE, GL_TRUE, GL_TRUE, GL_TRUE);
	glEnable(GL_BLEND);
	p_glBlendEquation(GL_FUNC_ADD);
	glBlendFunc(GL_SRC_ALPHA, GL_ONE_MINUS_SRC_ALPHA);
	p_glUseProgram(g_prog);
	p_glBindVertexArray(g_vao);
	float flip = (flags & 1) ? -1.0f : 1.0f;

	// Red: always on top of the 3D (no depth test).
	glDisable(GL_DEPTH_TEST);
	glDepthMask(GL_FALSE);
	draw_tri(flip, 0.0f, -0.42f, 0.05f, 0.30f, 1.0f, 0.1f, 0.1f, 1.0f);

	// Green: depth-tested against the scene (reverse-Z GEQUAL, no depth writes) behind the cube.
	if (flags & 4) {
		glEnable(GL_DEPTH_TEST);
		glDepthFunc(GL_GEQUAL);
	}
	draw_tri(flip, behind[2], 0.30f, -0.05f, 0.40f, 0.1f, 1.0f, 0.2f, 1.0f);
	// Blue: depth-tested, placed by view_proj in front of the cube.
	draw_tri(flip, front[2], front[0], front[1], 0.08f, 0.1f, 0.3f, 1.0f, 1.0f);

	// Leave GL dirty on purpose (what Ikemen's renderer could do): the module's guard must undo all of it.
	glDisable(GL_MULTISAMPLE);
	glFrontFace(GL_CW);
	glEnable(GL_CULL_FACE);
	glCullFace(GL_FRONT);
	glClearColor(1.0f, 0.0f, 1.0f, 1.0f);
	glPixelStorei(GL_UNPACK_ROW_LENGTH, 7);
	glPixelStorei(GL_UNPACK_ALIGNMENT, 1);
	glColorMask(GL_TRUE, GL_FALSE, GL_TRUE, GL_FALSE);
	p_glBlendEquation(GL_FUNC_REVERSE_SUBTRACT);
	glBlendFunc(GL_ONE, GL_ONE);
	glEnable(GL_SCISSOR_TEST);
	glScissor(0, 0, 8, 8);
	glEnable(GL_DEPTH_TEST);
	glDepthFunc(GL_NEVER);
	glDepthMask(GL_FALSE);
	glViewport(0, 0, 1, 1);
	p_glActiveTexture(GL_TEXTURE0 + 3);
	glBindTexture(GL_TEXTURE_2D, g_tex);
	p_glActiveTexture(GL_TEXTURE0);
	glBindTexture(GL_TEXTURE_2D, g_tex);
	p_glBindBuffer(GL_ARRAY_BUFFER, g_vbo);
	p_glBindFramebuffer(GL_FRAMEBUFFER, 0);
	g_renders++;
	return 0;
}

EXPORT int ikemen_command(const char *json) {
	g_commands++;
	char line[256];
	snprintf(line, sizeof(line), "{\"e\":\"cmd\",\"n\":%d,\"len\":%d}", g_commands, (int)strlen(json ? json : ""));
	push_event(line);
	return 0;
}

EXPORT int ikemen_state(float *buf, int cap_floats) {
	if (cap_floats < 32) {
		return 0;
	}
	memset(buf, 0, 32 * sizeof(float));
	buf[0] = 0; // handle
	buf[1] = 1 | 4 | 64; // alive, ctrl, ready
	buf[2] = 2080; // x
	buf[3] = 590; // depth
	buf[9] = 1000; // life
	buf[10] = 1000; // life max
	buf[14] = (float)g_ticks; // state time
	buf[12] = (float)g_renders; // (test) renders so far
	buf[13] = (float)g_pumps; // (test) pumps so far
	buf[15] = (float)g_budget_pumps; // (test) idle pumps so far (ikemen_gl_pump_budget)
	buf[16] = (float)g_pending; // (test) "uploads" still queued
	return 32;
}

EXPORT int ikemen_events(char *buf, int cap_bytes) {
	int n = (int)strlen(g_events);
	if (n > cap_bytes) {
		n = cap_bytes;
	}
	memcpy(buf, g_events, n);
	memmove(g_events, g_events + n, strlen(g_events + n) + 1);
	return n;
}

EXPORT const char *ikemen_last_error(void) {
	return g_error;
}

EXPORT int ikemen_status(void) {
	return g_status;
}

EXPORT void ikemen_shutdown(void) {
	g_status = 0;
}
