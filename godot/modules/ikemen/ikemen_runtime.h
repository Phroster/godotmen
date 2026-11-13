// Godotmen: Ikemen GO overlay module.
// Engine singleton "Ikemen": loads <project>/ikemen/bin/ikemen.dll (Go c-shared build of Ikemen GO),
// binds its C ABI, ticks it, and draws it straight into the root
// viewport's render target between the 3D pass and the HUD canvas layers (RendererViewport overlay hook).
// Every call is safe without the DLL: it returns an error code and last_error() says why.
#pragma once

#include "core/math/projection.h"
#include "core/object/class_db.h"
#include "core/object/object.h"
#include "core/os/mutex.h"
#include "core/variant/array.h"
#include "core/variant/dictionary.h"

class IkemenRuntime : public Object {
	GDCLASS(IkemenRuntime, Object);

public:
	// ikemen_render flags.
	enum {
		FLIP_Y = 1, // the target is stored bottom-up (every Godot render target): flip clip-space Y
		NO_CLEAR = 2, // draw onto the existing pixels (always set by the module)
		DEPTH_TEST = 4, // test against the scene's depth, reverse-Z (GL_GEQUAL), no depth writes
		DEBUG_CLSN = 8, // draw Ikemen's collision boxes
		SCENE_POST = 16, // ikemen_render_scene: Godot's post-processing encodes this 3D image afterwards
		SCENE_FRESH = 32, // ikemen_render: ikemen_render_scene ran on this frame's 3D pass
		SCENE_QUERY = 64, // ikemen_render_scene: only whether it would draw (1) or not (0), no GL call
		LATE_PASS = 128, // ikemen_render: drawn before Godot's transparent pass (solid sprites leave their depth for it)
	};

	// Fighter record size of get_state().
	enum {
		REC_FLOATS = 32,
		MAX_FIGHTERS = 128,
	};

	// Error codes of the module itself (the DLL's own codes are passed through unchanged).
	enum {
		IK_ERR_NO_DLL = -100, // ikemen.dll missing or LoadLibrary failed
		IK_ERR_MISSING_SYMBOL = -101, // the DLL lacks an export of the C ABI
		IK_ERR_NOT_STARTED = -102, // start() not called or failed
		IK_ERR_ALREADY_STARTED = -103,
		IK_ERR_NO_GL = -104, // the overlay needs the Compatibility (OpenGL 3.3) renderer
		IK_ERR_BAD_ARGUMENT = -105,
		IK_ERR_DLL_CALL = -106, // a DLL call failed without its own code
	};

private:
	static IkemenRuntime *singleton;

	struct Api {
		int (*init)(const char *, const char *) = nullptr;
		int (*gl_init)() = nullptr;
		void (*gl_pump)() = nullptr;
		int (*gl_pump_budget)(int) = nullptr; // optional (older DLLs: gl_pump): the idle pump's bounded drain
		int (*step)() = nullptr;
		int (*step_begin)() = nullptr;
		int (*step_end)() = nullptr;
		int (*render)(unsigned int, int, int, int, const float *, const float *) = nullptr;
		int (*render_scene)(unsigned int, int, int, int, int, int, const float *, const float *) = nullptr; // optional (older DLLs)
		int (*command)(const char *) = nullptr;
		int (*state)(float *, int) = nullptr;
		int (*events)(char *, int) = nullptr;
		const char *(*last_error)() = nullptr;
		int (*status)() = nullptr;
		void (*shutdown)() = nullptr;
		int (*mix_audio)(float *, int, int) = nullptr; // separate audio-thread callback, never dll_mutex
	};

	Api api;
	void *library = nullptr; // never closed: a Go c-shared runtime cannot be unloaded
	bool loaded = false; // DLL opened and every symbol bound
	bool load_attempted = false;
	bool started = false; // ikemen_init returned >= 0
	bool pipeline_enabled = false;
	String dll_file;
	String error;

	// Serialises host ABI calls. In pipeline mode simulation continues between
	// calls; render uses a pinned immutable snapshot and never the live world.
	Mutex dll_mutex;

	// Render-thread state (written through RenderingServer::call_on_render_thread).
	bool hook_installed = false;
	RID overlay_viewport;
	int overlay_first_layer = 0;
	int overlay_flags = 0;
	bool gl_inited = false;
	bool gl_failed = false;
	bool skip_guard = false; // IKEMEN_NO_GUARD=1 (diagnostics only)
	int gl_init_result = 0;
	float view_proj[16] = {};
	float params[16] = {};

	// Stats for tests and the debug HUD.
	uint64_t frames_drawn = 0;
	int last_render_result = 0;
	uint64_t last_render_usec = 0;
	int last_gl_errors = 0;
	uint64_t total_gl_errors = 0;
	int last_fbo = 0;
	Size2i last_size;

	// Scene hook (render thread): inside the root viewport's 3D pass - its opaque phase: the DLL works out the floor's
	// share of the 3D image (ikemen_render_scene, optional), the frame's draw is told it ran; its transparent phase
	// (Godot's transparent pass drawn late, on the render target): Ikemen's frame is drawn there (late_drawn), not by
	// the overlay hook.
	bool scene_ran = false;
	bool late_drawn = false;
	uint64_t scene_runs = 0;
	int last_scene_result = 0;
	uint64_t last_scene_usec = 0;

	// Idle pump (render thread): on frames the hook does not run - a minimized window draws nothing, so Main skips
	// RenderingServer::draw - the DLL's queued GL work (texture uploads of loading fighters) is still drained, bounded
	// per frame, so it never piles up and restoring the window never stalls on it. Driven from SceneTree's
	// process_frame (every main-loop iteration, drawn or not) through call_on_render_thread.
	static constexpr int IDLE_PUMP_USEC = 4000; // the most one idle frame spends on the queue
	static constexpr int IDLE_AFTER_FRAMES = 2; // frames in a row without the hook before the idle pump runs
	bool frame_connected = false; // main thread
	uint64_t hook_runs = 0; // _draw_overlay calls (render thread)
	uint64_t hook_runs_seen = 0;
	int idle_frames = 0; // frames in a row without the hook
	uint64_t idle_pumps = 0;
	int idle_backlog = 0; // tasks still queued after the last idle pump
	uint64_t idle_pump_usec_max = 0;

	void _on_process_frame();
	void _idle_pump_rt();
	bool _ensure_gl_init(); // render thread, dll_mutex held, inside the guard

	int _ensure_loaded();
	int _fail(int p_code, const String &p_message);
	int _check_started();
	int _dll_result(int p_result, const char *p_what);
	static bool _gl_available();
	String _resolve_dll_path() const;

	void _install_hook_rt(RID p_viewport, int p_first_layer, int p_flags);
	void _remove_hook_rt();
	static void _overlay_hook(RID p_viewport, RID p_render_target, void *p_userdata);
	void _draw_overlay(RID p_viewport, RID p_render_target);
	void _draw_ikemen(unsigned int p_fbo, Size2i p_size, int p_extra_flags, bool p_no_guard);
	static void _scene_hook(RID p_render_target, uint32_t p_fbo, int p_width, int p_height, bool p_post, int p_phase, void *p_userdata);
	void _draw_scene(RID p_render_target, uint32_t p_fbo, int p_width, int p_height, bool p_post);
	void _draw_late(unsigned int p_fbo, Size2i p_size);

protected:
	static void _bind_methods();

public:
	static IkemenRuntime *get_singleton() { return singleton; }

	int start(const String &p_ikemen_root, const Dictionary &p_config);
	int status();
	int command(const Dictionary &p_command);
	int step();
	int step_begin();
	int step_end();
	bool is_pipeline_enabled() const { return pipeline_enabled; }
	PackedFloat32Array get_state();
	Array poll_events();
	int set_overlay(RID p_viewport, int p_first_hud_layer, int p_flags);
	void clear_overlay();
	void set_camera(const Projection &p_view_proj, const PackedFloat32Array &p_params);
	String last_error() const;

	bool is_loaded() const { return loaded; }
	bool is_started() const { return started; }
	String get_dll_path() const;
	Dictionary get_render_stats() const;
	void shutdown();

	IkemenRuntime();
	~IkemenRuntime();
};
