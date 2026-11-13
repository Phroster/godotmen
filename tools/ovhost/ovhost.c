// ovhost: a native test host for ikemen/bin/ikemen.dll.
// Loads the DLL through its C ABI exactly the way Godot's module does: a hidden Win32 window
// with a WGL OpenGL 3.3 core context current on this (the "host") thread, ikemen_init / ikemen_gl_init, then per tick
// scripted inputs -> ikemen_step -> ikemen_render into the host's own FBO (cleared to a flat colour first, NO_CLEAR),
// with a perspective view-projection looking down at the floor. Writes PNGs every K ticks, logs timings, memory, state.
//
//   ovhost.exe [--parity] --root <ikemen game folder> --p1 <def> --p2 <def> [--ai2 4] [--ticks 900] [--every 60]
//              [--out <dir>] [--w 1280] [--h 720] [--stage stages/overlay.def] [--dll <ikemen.dll>]
//
// Always hidden (no window is ever shown), Ikemen without sound (IKEMEN_HIDDEN_RUN=1), stops itself after 100 s.

#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <psapi.h>
#include <GL/gl.h>
#include <math.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <zlib.h>

#define GL_FRAMEBUFFER 0x8D40
#define GL_RENDERBUFFER 0x8D41
#define GL_COLOR_ATTACHMENT0 0x8CE0
#define GL_DEPTH_ATTACHMENT 0x8D00
#define GL_DEPTH_COMPONENT24 0x81A6
#define GL_FRAMEBUFFER_COMPLETE 0x8CD5
#define GL_RGBA8 0x8058
#define WGL_CONTEXT_MAJOR_VERSION_ARB 0x2091
#define WGL_CONTEXT_MINOR_VERSION_ARB 0x2092
#define WGL_CONTEXT_PROFILE_MASK_ARB 0x9126
#define WGL_CONTEXT_CORE_PROFILE_BIT_ARB 0x00000001

typedef HGLRC(WINAPI *PFN_wglCreateContextAttribsARB)(HDC, HGLRC, const int *);
typedef void(APIENTRY *PFN_glGenFramebuffers)(GLsizei, GLuint *);
typedef void(APIENTRY *PFN_glBindFramebuffer)(GLenum, GLuint);
typedef void(APIENTRY *PFN_glFramebufferTexture2D)(GLenum, GLenum, GLenum, GLuint, GLint);
typedef void(APIENTRY *PFN_glGenRenderbuffers)(GLsizei, GLuint *);
typedef void(APIENTRY *PFN_glBindRenderbuffer)(GLenum, GLuint);
typedef void(APIENTRY *PFN_glRenderbufferStorage)(GLenum, GLenum, GLsizei, GLsizei);
typedef void(APIENTRY *PFN_glFramebufferRenderbuffer)(GLenum, GLenum, GLenum, GLuint);
typedef GLenum(APIENTRY *PFN_glCheckFramebufferStatus)(GLenum);

static PFN_glGenFramebuffers pglGenFramebuffers;
static PFN_glBindFramebuffer pglBindFramebuffer;
static PFN_glFramebufferTexture2D pglFramebufferTexture2D;
static PFN_glGenRenderbuffers pglGenRenderbuffers;
static PFN_glBindRenderbuffer pglBindRenderbuffer;
static PFN_glRenderbufferStorage pglRenderbufferStorage;
static PFN_glFramebufferRenderbuffer pglFramebufferRenderbuffer;
static PFN_glCheckFramebufferStatus pglCheckFramebufferStatus;

// --scene: a tiny 3D "arena" (floor + a pillar in front of the fighters' plane) drawn with depth before Ikemen,
// like Godot's 3D pass, so the DEPTH_TEST path (and its depth bias) can be checked against real geometry
typedef GLuint(APIENTRY *PFN_glCreateShader)(GLenum);
typedef void(APIENTRY *PFN_glShaderSource)(GLuint, GLsizei, const char *const *, const GLint *);
typedef void(APIENTRY *PFN_glCompileShader)(GLuint);
typedef GLuint(APIENTRY *PFN_glCreateProgram)(void);
typedef void(APIENTRY *PFN_glAttachShader)(GLuint, GLuint);
typedef void(APIENTRY *PFN_glLinkProgram)(GLuint);
typedef void(APIENTRY *PFN_glUseProgram)(GLuint);
typedef GLint(APIENTRY *PFN_glGetUniformLocation)(GLuint, const char *);
typedef void(APIENTRY *PFN_glUniformMatrix4fv)(GLint, GLsizei, GLboolean, const GLfloat *);
typedef void(APIENTRY *PFN_glGenVertexArrays)(GLsizei, GLuint *);
typedef void(APIENTRY *PFN_glBindVertexArray)(GLuint);
typedef void(APIENTRY *PFN_glGenBuffers)(GLsizei, GLuint *);
typedef void(APIENTRY *PFN_glBindBuffer)(GLenum, GLuint);
typedef void(APIENTRY *PFN_glBufferData)(GLenum, ptrdiff_t, const void *, GLenum);
typedef void(APIENTRY *PFN_glVertexAttribPointer)(GLuint, GLint, GLenum, GLboolean, GLsizei, const void *);
typedef void(APIENTRY *PFN_glEnableVertexAttribArray)(GLuint);
static PFN_glCreateShader pglCreateShader;
static PFN_glShaderSource pglShaderSource;
static PFN_glCompileShader pglCompileShader;
static PFN_glCreateProgram pglCreateProgram;
static PFN_glAttachShader pglAttachShader;
static PFN_glLinkProgram pglLinkProgram;
static PFN_glUseProgram pglUseProgram;
static PFN_glGetUniformLocation pglGetUniformLocation;
static PFN_glUniformMatrix4fv pglUniformMatrix4fv;
static PFN_glGenVertexArrays pglGenVertexArrays;
static PFN_glBindVertexArray pglBindVertexArray;
static PFN_glGenBuffers pglGenBuffers;
static PFN_glBindBuffer pglBindBuffer;
static PFN_glBufferData pglBufferData;
static PFN_glVertexAttribPointer pglVertexAttribPointer;
static PFN_glEnableVertexAttribArray pglEnableVertexAttribArray;
static GLuint scene_prog, scene_vao;
static GLint scene_vp_loc;
static int scene_verts;

// ikemen.dll's C ABI
typedef int (*p_init)(const char *, const char *);
typedef int (*p_int)(void);
typedef void (*p_void)(void);
typedef int (*p_render)(unsigned, int, int, int, const float *, const float *);
typedef int (*p_cmd)(const char *);
typedef int (*p_state)(float *, int);
typedef int (*p_events)(char *, int);
typedef const char *(*p_err)(void);

static p_init ikemen_init;
static p_int ikemen_gl_init, ikemen_step, ikemen_step_begin, ikemen_step_end, ikemen_status;
static p_void ikemen_gl_pump, ikemen_shutdown;
static p_render ikemen_render;
static p_cmd ikemen_command;
static p_state ikemen_state;
static p_events ikemen_events;
static p_err ikemen_last_error;

static LARGE_INTEGER qpf;
static double now_ms(void) {
	LARGE_INTEGER c;
	QueryPerformanceCounter(&c);
	return (double)c.QuadPart * 1000.0 / (double)qpf.QuadPart;
}

static void *gl_proc(const char *name) {
	void *p = (void *)wglGetProcAddress(name);
	if (p == 0 || p == (void *)1 || p == (void *)2 || p == (void *)3 || p == (void *)-1) {
		p = (void *)GetProcAddress(GetModuleHandleA("opengl32.dll"), name);
	}
	if (!p) {
		fprintf(stderr, "ovhost: missing GL function %s\n", name);
		exit(2);
	}
	return p;
}

// ------------------------------------------------------------------------------------------------------------------
// PNG (zlib)

static void put32(unsigned char *p, uint32_t v) {
	p[0] = v >> 24;
	p[1] = v >> 16;
	p[2] = v >> 8;
	p[3] = v;
}

static void png_chunk(FILE *f, const char *type, const unsigned char *data, uint32_t len) {
	unsigned char b[4];
	put32(b, len);
	fwrite(b, 1, 4, f);
	fwrite(type, 1, 4, f);
	if (len) fwrite(data, 1, len, f);
	uLong crc = crc32(0, (const Bytef *)type, 4);
	if (len) crc = crc32(crc, data, len);
	put32(b, (uint32_t)crc);
	fwrite(b, 1, 4, f);
}

// rgba: bottom-up rows (glReadPixels); alpha forced opaque
static int png_write(const char *path, unsigned char *rgba, int w, int h) {
	size_t stride = (size_t)w * 4 + 1, raw = stride * h;
	unsigned char *buf = malloc(raw);
	if (!buf) return -1;
	for (int y = 0; y < h; y++) {
		unsigned char *row = buf + y * stride;
		row[0] = 0;
		memcpy(row + 1, rgba + (size_t)(h - 1 - y) * w * 4, (size_t)w * 4);
		for (int x = 0; x < w; x++) row[1 + x * 4 + 3] = 255;
	}
	uLongf clen = compressBound(raw);
	unsigned char *comp = malloc(clen);
	if (!comp || compress2(comp, &clen, buf, raw, 6) != Z_OK) {
		free(buf); free(comp); return -1;
	}
	FILE *f = fopen(path, "wb");
	if (!f) {
		free(buf);
		free(comp);
		return -1;
	}
	static const unsigned char sig[8] = {137, 80, 78, 71, 13, 10, 26, 10};
	fwrite(sig, 1, 8, f);
	unsigned char ihdr[13];
	put32(ihdr, w);
	put32(ihdr + 4, h);
	ihdr[8] = 8;
	ihdr[9] = 6;
	ihdr[10] = ihdr[11] = ihdr[12] = 0;
	png_chunk(f, "IHDR", ihdr, 13);
	png_chunk(f, "IDAT", comp, (uint32_t)clen);
	png_chunk(f, "IEND", NULL, 0);
	int failed = ferror(f);
	if (fclose(f)) failed = 1;
	free(buf);
	free(comp);
	return failed ? -1 : 0;
}

// ------------------------------------------------------------------------------------------------------------------
// Matrices (column-major, like Godot's Projection * Transform handed to ikemen_render)

static void mat_mul(float *out, const float *a, const float *b) {
	float r[16];
	for (int c = 0; c < 4; c++)
		for (int rr = 0; rr < 4; rr++) {
			float s = 0;
			for (int k = 0; k < 4; k++) s += a[k * 4 + rr] * b[c * 4 + k];
			r[c * 4 + rr] = s;
		}
	memcpy(out, r, sizeof r);
}

static void perspective(float *m, float fovy_deg, float aspect, float zn, float zf) {
	float f = 1.0f / tanf(fovy_deg * 3.14159265f / 360.0f);
	memset(m, 0, 16 * sizeof(float));
	m[0] = f / aspect;
	m[5] = f;
	m[10] = (zf + zn) / (zn - zf);
	m[11] = -1;
	m[14] = 2 * zf * zn / (zn - zf);
}

static void look_at(float *m, const float *eye, const float *at) {
	float fx = at[0] - eye[0], fy = at[1] - eye[1], fz = at[2] - eye[2];
	float fl = sqrtf(fx * fx + fy * fy + fz * fz);
	fx /= fl, fy /= fl, fz /= fl;
	float sx = fy * 0 - fz * 1, sy = fz * 0 - fx * 0, sz = fx * 1 - fy * 0; // f x up(0,1,0)
	float sl = sqrtf(sx * sx + sy * sy + sz * sz);
	sx /= sl, sy /= sl, sz /= sl;
	float ux = sy * fz - sz * fy, uy = sz * fx - sx * fz, uz = sx * fy - sy * fx;
	memset(m, 0, 16 * sizeof(float));
	m[0] = sx, m[4] = sy, m[8] = sz;
	m[1] = ux, m[5] = uy, m[9] = uz;
	m[2] = -fx, m[6] = -fy, m[10] = -fz;
	m[12] = -(sx * eye[0] + sy * eye[1] + sz * eye[2]);
	m[13] = -(ux * eye[0] + uy * eye[1] + uz * eye[2]);
	m[14] = (fx * eye[0] + fy * eye[1] + fz * eye[2]);
	m[15] = 1;
}

// ------------------------------------------------------------------------------------------------------------------

static void *gl_proc(const char *name);

static void scene_init(void) {
	pglCreateShader = gl_proc("glCreateShader");
	pglShaderSource = gl_proc("glShaderSource");
	pglCompileShader = gl_proc("glCompileShader");
	pglCreateProgram = gl_proc("glCreateProgram");
	pglAttachShader = gl_proc("glAttachShader");
	pglLinkProgram = gl_proc("glLinkProgram");
	pglUseProgram = gl_proc("glUseProgram");
	pglGetUniformLocation = gl_proc("glGetUniformLocation");
	pglUniformMatrix4fv = gl_proc("glUniformMatrix4fv");
	pglGenVertexArrays = gl_proc("glGenVertexArrays");
	pglBindVertexArray = gl_proc("glBindVertexArray");
	pglGenBuffers = gl_proc("glGenBuffers");
	pglBindBuffer = gl_proc("glBindBuffer");
	pglBufferData = gl_proc("glBufferData");
	pglVertexAttribPointer = gl_proc("glVertexAttribPointer");
	pglEnableVertexAttribArray = gl_proc("glEnableVertexAttribArray");
	const char *vs = "#version 330 core\nlayout(location=0) in vec3 p; layout(location=1) in vec3 c; uniform mat4 vp;"
		"out vec3 vc; out vec3 wp; void main(){ vc=c; wp=p; gl_Position = vp*vec4(p,1.0); }";
	const char *fs = "#version 330 core\nin vec3 vc; in vec3 wp; out vec4 o; void main(){ float k = 1.0;"
		"if (vc.r < 0.0) { k = (mod(floor(wp.x) + floor(wp.z), 2.0) < 1.0) ? 0.30 : 0.38; o = vec4(k, k*0.95, k*0.9, 1.0); }"
		"else o = vec4(vc, 1.0); }";
	GLuint v = pglCreateShader(0x8B31 /*GL_VERTEX_SHADER*/), f = pglCreateShader(0x8B30 /*GL_FRAGMENT_SHADER*/);
	pglShaderSource(v, 1, &vs, NULL);
	pglCompileShader(v);
	pglShaderSource(f, 1, &fs, NULL);
	pglCompileShader(f);
	scene_prog = pglCreateProgram();
	pglAttachShader(scene_prog, v);
	pglAttachShader(scene_prog, f);
	pglLinkProgram(scene_prog);
	scene_vp_loc = pglGetUniformLocation(scene_prog, "vp");
	// a floor at y = 0; a 0.5 x 3 x 0.5 m pillar 1.2 m in front of the
	// fighters' starting plane (z 0), right of centre
	float px0 = 0.6f, px1 = px0 + 0.5f, pz0 = 1.2f, pz1 = pz0 + 0.5f, ph = 3.0f;
	float c0 = 0.45f, c1 = 0.30f, c2 = 0.20f;
	float d[] = {
		0, 0, 0, -1, 0, 0, 62.4f, 0, 0, -1, 0, 0, 62.4f, 0, 18.72f, -1, 0, 0,
		0, 0, 0, -1, 0, 0, 62.4f, 0, 18.72f, -1, 0, 0, 0, 0, 18.72f, -1, 0, 0,
		// pillar: front (+z), left, right, top
		px0, 0, pz1, c0, c1, c2, px1, 0, pz1, c0, c1, c2, px1, ph, pz1, c0, c1, c2,
		px0, 0, pz1, c0, c1, c2, px1, ph, pz1, c0, c1, c2, px0, ph, pz1, c0, c1, c2,
		px0, 0, pz0, c0 * .7f, c1 * .7f, c2 * .7f, px0, 0, pz1, c0 * .7f, c1 * .7f, c2 * .7f, px0, ph, pz1, c0 * .7f, c1 * .7f, c2 * .7f,
		px0, 0, pz0, c0 * .7f, c1 * .7f, c2 * .7f, px0, ph, pz1, c0 * .7f, c1 * .7f, c2 * .7f, px0, ph, pz0, c0 * .7f, c1 * .7f, c2 * .7f,
		px1, 0, pz1, c0 * .8f, c1 * .8f, c2 * .8f, px1, 0, pz0, c0 * .8f, c1 * .8f, c2 * .8f, px1, ph, pz0, c0 * .8f, c1 * .8f, c2 * .8f,
		px1, 0, pz1, c0 * .8f, c1 * .8f, c2 * .8f, px1, ph, pz0, c0 * .8f, c1 * .8f, c2 * .8f, px1, ph, pz1, c0 * .8f, c1 * .8f, c2 * .8f,
		px0, ph, pz1, c0, c1, c2, px1, ph, pz1, c0, c1, c2, px1, ph, pz0, c0, c1, c2,
		px0, ph, pz1, c0, c1, c2, px1, ph, pz0, c0, c1, c2, px0, ph, pz0, c0, c1, c2,
	};
	scene_verts = (int)(sizeof d / sizeof d[0] / 6);
	GLuint vbo;
	pglGenVertexArrays(1, &scene_vao);
	pglBindVertexArray(scene_vao);
	pglGenBuffers(1, &vbo);
	pglBindBuffer(0x8892 /*GL_ARRAY_BUFFER*/, vbo);
	pglBufferData(0x8892, sizeof d, d, 0x88E4 /*GL_STATIC_DRAW*/);
	pglVertexAttribPointer(0, 3, GL_FLOAT, GL_FALSE, 24, (void *)0);
	pglEnableVertexAttribArray(0);
	pglVertexAttribPointer(1, 3, GL_FLOAT, GL_FALSE, 24, (void *)12);
	pglEnableVertexAttribArray(1);
	pglBindVertexArray(0);
}

// The host's 3D pass: standard depth (LEQUAL, cleared to 1), depth writes on
static int revz; // --revz 1: the host's 3D pass is Godot-like reverse-Z (depth cleared to 0, GL_GEQUAL)

// The host's depth clear value: 1 (far) for standard depth, 0 (far) for reverse-Z
static double depth_far(void) { return revz ? 0.0 : 1.0; }

static void scene_draw(const float *vp) {
	float corrected[16];
	if (revz) { // Godot: Projection::set_depth_correction(flip_y = false here, reverse_z = true, remap_z = false) * P * V
		memcpy(corrected, vp, sizeof corrected);
		for (int c = 0; c < 4; c++) corrected[c * 4 + 2] = -corrected[c * 4 + 2];
		vp = corrected;
	}
	glEnable(GL_DEPTH_TEST);
	glDepthFunc(revz ? GL_GEQUAL : GL_LEQUAL);
	glDepthMask(GL_TRUE);
	pglUseProgram(scene_prog);
	pglUniformMatrix4fv(scene_vp_loc, 1, GL_FALSE, vp);
	pglBindVertexArray(scene_vao);
	glDrawArrays(GL_TRIANGLES, 0, scene_verts);
	pglBindVertexArray(0);
	pglUseProgram(0);
	glDisable(GL_DEPTH_TEST);
}

static LRESULT CALLBACK wndproc(HWND h, UINT m, WPARAM w, LPARAM l) { return DefWindowProcA(h, m, w, l); }

static const char *arg(int argc, char **argv, const char *name, const char *def) {
	for (int i = 1; i + 1 < argc; i++)
		if (!strcmp(argv[i], name)) return argv[i + 1];
	return def;
}

static int has(int argc, char **argv, const char *name) {
	for (int i=1; i<argc; i++) if (!strcmp(argv[i], name)) return 1;
	return 0;
}

static void slashes(char *s) {
	for (; *s; s++)
		if (*s == '\\') *s = '/';
}

static size_t mem_private(void) {
	PROCESS_MEMORY_COUNTERS_EX pmc;
	GetProcessMemoryInfo(GetCurrentProcess(), (PROCESS_MEMORY_COUNTERS *)&pmc, sizeof pmc);
	return pmc.PrivateUsage;
}

// Scripted host input for P1 (handle 0): Ikemen's 14 bits, absolute L/R
static int script_bits(int t) {
	const int U = 1, D = 2, L = 4, R = 8, a = 16, b = 32, c = 64, x = 128, y = 256;
	(void)U, (void)c;
	if (t < 200) return 0;                            // stand through the round intro (no ctrl yet)
	if (t < 280) return L;                            // walk left (away from P2)
	if (t < 330) return R;                            // walk right (toward P2)
	if (t < 400) return ((t / 12) % 2) ? x : 0;       // tap x
	if (t < 460) return ((t / 10) % 2) ? a : 0;       // tap a
	if (t < 500) return D;                            // crouch
	if (t < 560) return ((t / 10) % 2) ? (D | y) : D; // crouching y
	if (t < 620) return ((t / 8) % 2) ? b : 0;        // tap b
	if (t < 660) return U | R;                        // jump forward
	return ((t / 15) % 3 == 0) ? x : (((t / 15) % 3 == 1) ? y : R);
}


int main(int argc, char **argv) {
	QueryPerformanceFrequency(&qpf);
	double t_start = now_ms();
	const char *root = arg(argc, argv, "--root", NULL);
	const char *p1 = arg(argc, argv, "--p1", NULL);
	const char *p2 = arg(argc, argv, "--p2", NULL);
	int parity = has(argc, argv, "--parity"); // stock match for the parity test: no host input, no host commands
	if (!root || (!parity && (!p1 || !p2))) {
		fprintf(stderr, "usage: ovhost --root <dir> --p1 <def> --p2 <def> [--ai2 N] [--ticks N] [--every K] [--out dir]\n");
		return 1;
	}
	int scene = atoi(arg(argc, argv, "--scene", "0"));        // 1: floor + pillar with depth, DEPTH_TEST on
	float bias = (float)atof(arg(argc, argv, "--bias", "0")); // params16[7] depth bias (m) in scene mode
	int ai1 = atoi(arg(argc, argv, "--ai1", "0"));
	float depth2 = (float)atof(arg(argc, argv, "--depth2", "0")); // P2's floor depth (op set_pos) after the first tick
	revz = atoi(arg(argc, argv, "--revz", "0"));
	int ai2 = atoi(arg(argc, argv, "--ai2", "4"));
	int ticks = atoi(arg(argc, argv, "--ticks", "900"));
	int every = atoi(arg(argc, argv, "--every", "60"));
	int W = atoi(arg(argc, argv, "--w", "1280")), H = atoi(arg(argc, argv, "--h", "720"));
	const char *out = arg(argc, argv, "--out", "ovhost_out");
	const char *stage = arg(argc, argv, "--stage", "stages/overlay.def");
	double limit_ms = atof(arg(argc, argv, "--limit", "100")) * 1000.0;
	if (limit_ms <= 0 || !isfinite(limit_ms)) { fprintf(stderr, "ovhost: invalid wall limit\n"); return 1; }
	if (limit_ms > 105000.0) limit_ms = 105000.0;
	char dllpath[MAX_PATH];
	GetModuleFileNameA(NULL, dllpath, MAX_PATH);
	char *slash = strrchr(dllpath, '\\');
	if (slash) strcpy(slash + 1, "ikemen.dll");
	const char *dll = arg(argc, argv, "--dll", dllpath);

	SetEnvironmentVariableA("IKEMEN_HIDDEN_RUN", "1");

	// Hidden window + WGL 3.3 core context on this thread (the host thread, like Godot's main thread)
	WNDCLASSA wc = {0};
	wc.style = CS_OWNDC;
	wc.lpfnWndProc = wndproc;
	wc.hInstance = GetModuleHandleA(NULL);
	wc.lpszClassName = "ikemen_ovhost";
	RegisterClassA(&wc);
	HWND hwnd = CreateWindowExA(0, "ikemen_ovhost", "ovhost", WS_OVERLAPPEDWINDOW, 0, 0, 64, 64, NULL, NULL, wc.hInstance, NULL);
	HDC dc = GetDC(hwnd);
	PIXELFORMATDESCRIPTOR pfd = {sizeof pfd, 1, PFD_DRAW_TO_WINDOW | PFD_SUPPORT_OPENGL | PFD_DOUBLEBUFFER, PFD_TYPE_RGBA, 32};
	pfd.cDepthBits = 24;
	pfd.cStencilBits = 8;
	pfd.cAlphaBits = 8;
	SetPixelFormat(dc, ChoosePixelFormat(dc, &pfd), &pfd);
	HGLRC legacy = wglCreateContext(dc);
	wglMakeCurrent(dc, legacy);
	PFN_wglCreateContextAttribsARB wglCreateContextAttribsARB = (PFN_wglCreateContextAttribsARB)wglGetProcAddress("wglCreateContextAttribsARB");
	int attribs[] = {WGL_CONTEXT_MAJOR_VERSION_ARB, 3, WGL_CONTEXT_MINOR_VERSION_ARB, 3, WGL_CONTEXT_PROFILE_MASK_ARB, WGL_CONTEXT_CORE_PROFILE_BIT_ARB, 0};
	HGLRC ctx = wglCreateContextAttribsARB(dc, NULL, attribs);
	wglMakeCurrent(dc, ctx);
	wglDeleteContext(legacy);
	printf("ovhost: GL %s | %s\n", glGetString(GL_VERSION), glGetString(GL_RENDERER));

	pglGenFramebuffers = gl_proc("glGenFramebuffers");
	pglBindFramebuffer = gl_proc("glBindFramebuffer");
	pglFramebufferTexture2D = gl_proc("glFramebufferTexture2D");
	pglGenRenderbuffers = gl_proc("glGenRenderbuffers");
	pglBindRenderbuffer = gl_proc("glBindRenderbuffer");
	pglRenderbufferStorage = gl_proc("glRenderbufferStorage");
	pglFramebufferRenderbuffer = gl_proc("glFramebufferRenderbuffer");
	pglCheckFramebufferStatus = gl_proc("glCheckFramebufferStatus");
	if (scene) scene_init();

	// The host's render target: RGBA8 colour + 24-bit depth (like a root viewport)
	GLuint tex, rbo, fbo;
	glGenTextures(1, &tex);
	glBindTexture(GL_TEXTURE_2D, tex);
	glTexImage2D(GL_TEXTURE_2D, 0, GL_RGBA8, W, H, 0, GL_RGBA, GL_UNSIGNED_BYTE, NULL);
	glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MIN_FILTER, GL_NEAREST);
	glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MAG_FILTER, GL_NEAREST);
	glBindTexture(GL_TEXTURE_2D, 0);
	pglGenRenderbuffers(1, &rbo);
	pglBindRenderbuffer(GL_RENDERBUFFER, rbo);
	pglRenderbufferStorage(GL_RENDERBUFFER, GL_DEPTH_COMPONENT24, W, H);
	pglGenFramebuffers(1, &fbo);
	pglBindFramebuffer(GL_FRAMEBUFFER, fbo);
	pglFramebufferTexture2D(GL_FRAMEBUFFER, GL_COLOR_ATTACHMENT0, GL_TEXTURE_2D, tex, 0);
	pglFramebufferRenderbuffer(GL_FRAMEBUFFER, GL_DEPTH_ATTACHMENT, GL_RENDERBUFFER, rbo);
	if (pglCheckFramebufferStatus(GL_FRAMEBUFFER) != GL_FRAMEBUFFER_COMPLETE) {
		fprintf(stderr, "ovhost: FBO incomplete\n");
		return 2;
	}
	pglBindFramebuffer(GL_FRAMEBUFFER, 0);

	// ikemen.dll, its SDL2/libxmp next to it
	HMODULE lib = LoadLibraryExA(dll, NULL, LOAD_WITH_ALTERED_SEARCH_PATH);
	if (!lib) {
		fprintf(stderr, "ovhost: LoadLibrary %s failed (%lu)\n", dll, GetLastError());
		return 2;
	}
#define LOAD(n)                                             \
	n = (void *)GetProcAddress(lib, #n);                   \
	if (!n) {                                              \
		fprintf(stderr, "ovhost: %s not exported\n", #n); \
		return 2;                                          \
	}
	LOAD(ikemen_init) LOAD(ikemen_gl_init) LOAD(ikemen_gl_pump) LOAD(ikemen_step) LOAD(ikemen_render) LOAD(ikemen_command)
	LOAD(ikemen_state) LOAD(ikemen_events) LOAD(ikemen_last_error) LOAD(ikemen_status) LOAD(ikemen_shutdown)
	ikemen_step_begin = (void *)GetProcAddress(lib, "ikemen_step_begin");
	ikemen_step_end = (void *)GetProcAddress(lib, "ikemen_step_end");
	printf("ovhost: loaded %s (%.0f ms), private %.1f MB\n", dll, now_ms() - t_start, mem_private() / 1048576.0);

	const char *p3 = arg(argc, argv, "--p3", NULL), *p4 = arg(argc, argv, "--p4", NULL);
	char d1[1024], d2[1024], st[1024], cfg[8192], extra[3072] = "";
	if (p3 && p4) { // 2v2 (Ikemen simul), both extra fighters on AI level 8
		char d3[1024], d4[1024];
		snprintf(d3, sizeof d3, "%s", p3);
		snprintf(d4, sizeof d4, "%s", p4);
		slashes(d3), slashes(d4);
		snprintf(extra, sizeof extra, ",{\"def\":\"%s\",\"pal\":2,\"ai\":8},{\"def\":\"%s\",\"pal\":2,\"ai\":8}", d3, d4);
	}
	snprintf(d1, sizeof d1, "%s", p1);
	snprintf(d2, sizeof d2, "%s", p2);
	snprintf(st, sizeof st, "%s", stage);
	slashes(d1), slashes(d2), slashes(st);
	snprintf(cfg, sizeof cfg,
		"{\"fighters\":[{\"def\":\"%s\",\"pal\":1,\"ai\":%d},{\"def\":\"%s\",\"pal\":1,\"ai\":%d}%s],"
		"\"stage\":\"%s\",\"nosound\":true,\"round_time\":-1,\"gomaxprocs\":2%s}",
		d1, ai1, d2, ai2, extra, st, parity ? ",\"parity_mode\":true" : "");
	// ikemen_init failing before the Ikemen thread starts must leave the DLL ready for a retry in the same process
	int bad1 = ikemen_init("Z:/no/such/ikemen/root", cfg);
	int bad2 = ikemen_init(root, "{\"fighters\":[]}");
	printf("ovhost: failing inits returned %d %d (expect -1 -1), last error '%s'\n", bad1, bad2, ikemen_last_error());
	if (ikemen_init(root, cfg) != 0) {
		fprintf(stderr, "ovhost: ikemen_init failed: %s\n", ikemen_last_error());
		return 3;
	}
	if (ikemen_gl_init() != 0) {
		fprintf(stderr, "ovhost: ikemen_gl_init failed: %s\n", ikemen_last_error());
		return 3;
	}
	// Booting: the host keeps ticking and drawing like Godot does (step returns 0, render drains texture work, draws
	// nothing), with a plain gl_pump in between
	double t_boot = now_ms();
	float boot_vp[16] = {1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}, boot_params[16] = {0.015f, 0.0f, 0.0f, 1.0f};
	int boot_frames = 0, boot_steps = 0;
	while (ikemen_status() == 0 && now_ms() - t_start < limit_ms) {
		boot_steps += ikemen_step();
		pglBindFramebuffer(GL_FRAMEBUFFER, fbo);
		glClear(GL_COLOR_BUFFER_BIT);
		ikemen_render(fbo, W, H, 2, boot_vp, boot_params);
		ikemen_gl_pump();
		boot_frames++;
		Sleep(16);
	}
	printf("ovhost: %d host frames while booting (steps that ticked: %d)\n", boot_frames, boot_steps);
	if (ikemen_status() != 1) {
		fprintf(stderr, "ovhost: boot failed (status %d): %s\n", ikemen_status(), ikemen_last_error());
		ikemen_shutdown();
		ExitProcess(4);
	}
	printf("ovhost: match ready after %.0f ms, private %.1f MB\n", now_ms() - t_boot, mem_private() / 1048576.0);

	// Arena-like camera: 6.5 m back, 0.33 m up per metre, aiming 1.5 m above the floor at ik (0, 0, 0)
	float params[16] = {0.015f, 0.0f, 0.0f, 1.0f, 0.0f};
	float proj[16], view[16], vp[16];
	float target[3] = {0.0f, 1.5f, 0.0f}, eye[3] = {0.0f, 1.5f + 6.5f * 0.33f, 6.5f};
	perspective(proj, 48.0f, (float)W / (float)H, 0.3f, 400.0f);
	look_at(view, eye, target);
	mat_mul(vp, proj, view);

	unsigned char *pix = malloc((size_t)W * H * 4);
	static char evbuf[1 << 16];
	static float sbuf[32 * 16];
	double step_sum = 0, step_max = 0, rend_sum = 0, rend_max = 0, fin_sum = 0;
	int nevents = 0, nrend = 0, last_bits = -1, gl_errors = 0, state_bad = 0;
	size_t mem0 = 0;
	char path[1024];
	for (int t = 0; t < ticks && now_ms() - t_start < limit_ms; t++) {
		int bits = script_bits(t);
		if (!parity && bits != last_bits) {
			char cmd[128];
			snprintf(cmd, sizeof cmd, "{\"op\":\"input\",\"h\":0,\"bits\":%d}", bits);
			if (ikemen_command(cmd) < 0) fprintf(stderr, "ovhost: input failed: %s\n", ikemen_last_error());
			last_bits = bits;
		}
		if (t == 1 && depth2 != 0) {
			char cmd[160];
			snprintf(cmd, sizeof cmd, "{\"op\":\"set_pos\",\"h\":1,\"x\":60,\"depth\":%.1f}", depth2);
			if (ikemen_command(cmd) < 0) fprintf(stderr, "ovhost: set_pos failed: %s\n", ikemen_last_error());
		}
		if (t == 100 && !parity) { // pause: steps must not tick
			ikemen_command("{\"op\":\"pause\",\"on\":true}");
			int p0 = ikemen_step(), p1s = ikemen_step();
			ikemen_command("{\"op\":\"pause\",\"on\":false}");
			printf("ovhost: paused steps returned %d %d (expect 0 0)\n", p0, p1s);
		}
		double a = now_ms();
		int r = ikemen_step();
		double b = now_ms();
		if (r < 0) {
			fprintf(stderr, "ovhost: step %d failed: %s\n", t, ikemen_last_error());
			break;
		}
		step_sum += b - a;
		if (b - a > step_max) step_max = b - a;

		int n = ikemen_events(evbuf, sizeof evbuf - 1);
		if (n > 0) {
			evbuf[n] = 0;
			for (char *p = evbuf; *p; p++)
				if (*p == '\n') nevents++;
			if (nevents < 40) printf("ovhost: t%d events: %s", t, evbuf);
		}

		// Camera follows the fighters' midpoint
		int nf0 = ikemen_state(sbuf, 32 * 16);
		if (nf0 >= 64) {
			float mx = 0, mz = 0;
			int cnt = 0;
			for (int i = 0; i + 32 <= nf0; i += 32, cnt++) {
				mx += sbuf[i + 2] * 0.015f;
				mz += sbuf[i + 3] * 0.015f;
			}
			mx /= cnt, mz /= cnt;
			target[0] += (mx - target[0]) * 0.1f;
			target[2] += (mz - target[2]) * 0.1f;
			eye[0] = target[0], eye[1] = target[1] + 6.5f * 0.33f, eye[2] = target[2] + 6.5f;
			look_at(view, eye, target);
			mat_mul(vp, proj, view);
		}

		// One host frame: clear to a flat colour, Ikemen draws on top (NO_CLEAR)
		pglBindFramebuffer(GL_FRAMEBUFFER, fbo);
		glViewport(0, 0, W, H);
		glClearColor(0.22f, 0.30f, 0.38f, 1.0f);
		glClearDepth(depth_far());
		glClear(GL_COLOR_BUFFER_BIT | GL_DEPTH_BUFFER_BIT);
		int rflags = 2; // NO_CLEAR
		if (scene) { // host 3D pass first, then Ikemen depth-tested against it (params[6] = 1: standard depth)
			if (scene != 2) scene_draw(vp);
			if (scene != 3) rflags |= 4;
			params[6] = revz ? 0.0f : 1.0f;
			params[7] = bias;
		}
		double c0 = now_ms();
		r = ikemen_render(fbo, W, H, rflags, vp, params);
		double c1 = now_ms();
		glFinish();
		double c2 = now_ms();
		if (r < 0) fprintf(stderr, "ovhost: render %d failed: %s\n", t, ikemen_last_error());
		// What the overlay leaves behind: no GL error, the host's FBO still bound, no program/blend/depth test on
		GLenum ge = glGetError();
		if (ge != GL_NO_ERROR) {
			if (gl_errors < 5) fprintf(stderr, "ovhost: GL error 0x%x after render %d\n", ge, t);
			gl_errors++;
		}
		GLint fb = -1, prog = -1;
		glGetIntegerv(0x8CA6 /*GL_FRAMEBUFFER_BINDING*/, &fb);
		glGetIntegerv(0x8B8D /*GL_CURRENT_PROGRAM*/, &prog);
		if (fb != (GLint)fbo || prog != 0 || glIsEnabled(GL_BLEND) || glIsEnabled(GL_DEPTH_TEST) || glIsEnabled(GL_SCISSOR_TEST))
			state_bad++;
		rend_sum += c1 - c0;
		fin_sum += c2 - c1;
		if (c1 - c0 > rend_max) rend_max = c1 - c0;
		nrend++;

		if (t == 120) mem0 = mem_private();
		if (every > 0 && t % every == 0) {
			pglBindFramebuffer(GL_FRAMEBUFFER, fbo);
			glReadPixels(0, 0, W, H, GL_RGBA, GL_UNSIGNED_BYTE, pix);
			snprintf(path, sizeof path, "%s/ov_t%04d.png", out, t);
			png_write(path, pix, W, H);
			int nf = ikemen_state(sbuf, 32 * 16);
			printf("ovhost: t%d bits %d step %.2f ms render %.2f ms private %.1f MB |", t, bits, b - a, c1 - c0,
				mem_private() / 1048576.0);
			for (int i = 0; i + 32 <= nf; i += 32)
				printf(" h%d x%.0f d%.0f h%.0f life%.0f st%.0f anim%.0f;", (int)sbuf[i], sbuf[i + 2], sbuf[i + 3],
					sbuf[i + 4], sbuf[i + 9], sbuf[i + 12], sbuf[i + 17]);
			printf("\n");
			fflush(stdout);
		}
	}

	// Depth-test frame: host depth = 1.0 (nearest, reverse-Z) on the left half, 0.0 on the right; GEQUAL, no writes
	pglBindFramebuffer(GL_FRAMEBUFFER, fbo);
	glClearColor(0.22f, 0.30f, 0.38f, 1.0f);
	glClear(GL_COLOR_BUFFER_BIT);
	glEnable(GL_SCISSOR_TEST);
	glScissor(0, 0, W / 2, H);
	glClearDepth(1.0);
	glClear(GL_DEPTH_BUFFER_BIT);
	glScissor(W / 2, 0, W - W / 2, H);
	glClearDepth(0.0);
	glClear(GL_DEPTH_BUFFER_BIT);
	glDisable(GL_SCISSOR_TEST);
	ikemen_render(fbo, W, H, 2 | 4, vp, params);
	pglBindFramebuffer(GL_FRAMEBUFFER, fbo);
	glReadPixels(0, 0, W, H, GL_RGBA, GL_UNSIGNED_BYTE, pix);
	snprintf(path, sizeof path, "%s/ov_depthtest.png", out);
	png_write(path, pix, W, H);

	// Scene mode: the same game state drawn with depth bias 0 and 0.3 m (floor contact of feet / ground effects)
	if (scene) {
		float biases[2] = {0.0f, 0.3f};
		for (int i = 0; i < 2; i++) {
			pglBindFramebuffer(GL_FRAMEBUFFER, fbo);
			glClearColor(0.22f, 0.30f, 0.38f, 1.0f);
			glClearDepth(depth_far());
			glClear(GL_COLOR_BUFFER_BIT | GL_DEPTH_BUFFER_BIT);
			scene_draw(vp);
			params[6] = revz ? 0.0f : 1.0f;
			params[7] = biases[i];
			ikemen_render(fbo, W, H, 2 | 4, vp, params);
			pglBindFramebuffer(GL_FRAMEBUFFER, fbo);
			glReadPixels(0, 0, W, H, GL_RGBA, GL_UNSIGNED_BYTE, pix);
			snprintf(path, sizeof path, "%s/ov_scene%s_bias%d.png", out, revz ? "_revz" : "", (int)(biases[i] * 10));
			png_write(path, pix, W, H);
		}
		params[6] = params[7] = 0.0f;
	}

	// DEBUG_CLSN frame (boxes are cued from the tick after the flag is first seen)
	ikemen_render(fbo, W, H, 2 | 8, vp, params);
	ikemen_step();
	pglBindFramebuffer(GL_FRAMEBUFFER, fbo);
	glClearColor(0.22f, 0.30f, 0.38f, 1.0f);
	glClear(GL_COLOR_BUFFER_BIT | GL_DEPTH_BUFFER_BIT);
	ikemen_render(fbo, W, H, 2 | 8, vp, params);
	pglBindFramebuffer(GL_FRAMEBUFFER, fbo);
	glReadPixels(0, 0, W, H, GL_RGBA, GL_UNSIGNED_BYTE, pix);
	snprintf(path, sizeof path, "%s/ov_clsn.png", out);
	png_write(path, pix, W, H);

	// Flip-Y + clear-by-Ikemen frame
	ikemen_render(fbo, W, H, 1, vp, params);
	pglBindFramebuffer(GL_FRAMEBUFFER, fbo);
	glReadPixels(0, 0, W, H, GL_RGBA, GL_UNSIGNED_BYTE, pix);
	snprintf(path, sizeof path, "%s/ov_flipy_clear.png", out);
	png_write(path, pix, W, H);

	printf("ovhost: %d frames | step avg %.3f ms max %.2f | render(cpu) avg %.3f ms max %.2f | glFinish avg %.3f ms"
		" | events %d | GL errors %d, state leaks %d | private %.1f MB (t120 %.1f MB) | total %.1f s | status %d err '%s'\n",
		nrend, nrend ? step_sum / nrend : 0, step_max, nrend ? rend_sum / nrend : 0, rend_max, nrend ? fin_sum / nrend : 0,
		nevents, gl_errors, state_bad, mem_private() / 1048576.0, mem0 / 1048576.0, (now_ms() - t_start) / 1000.0, ikemen_status(), ikemen_last_error());
	fflush(stdout);
	ikemen_shutdown();
	ExitProcess(0); // never FreeLibrary a Go DLL
}
