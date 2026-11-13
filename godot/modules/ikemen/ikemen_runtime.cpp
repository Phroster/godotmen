// Godotmen: Ikemen GO overlay module. See ikemen_runtime.h.
#include "ikemen_runtime.h"

#include "gl_state_guard.h"
#include "audio_stream_ikemen.h"

#include "core/config/project_settings.h"
#include "core/io/file_access.h"
#include "core/io/json.h"
#include "core/object/callable_mp.h"
#include "core/os/os.h"
#include "drivers/gles3/rasterizer_scene_gles3.h"
#include "drivers/gles3/storage/texture_storage.h"
#include "scene/main/scene_tree.h"
#include "servers/rendering/renderer_viewport.h"
#include "servers/rendering/rendering_server.h"
#include "servers/rendering/rendering_server_globals.h"

#include <type_traits>

IkemenRuntime *IkemenRuntime::singleton = nullptr;

// Default DLL location, relative to the project folder (res://). IKEMEN_DLL (absolute path) overrides it.
static const char *IKEMEN_DLL_RES_PATH = "res://ikemen/bin/ikemen.dll";
static const char *IKEMEN_ROOT_RES_PATH = "res://ikemen";

// Generous buffers: 128 fighters x 32 floats; events are drained in 256 KiB chunks.
static constexpr int EVENT_CHUNK_BYTES = 256 * 1024;
static constexpr int EVENT_MAX_CHUNKS = 16;

IkemenRuntime::IkemenRuntime() {
	singleton = this;
	// Identity camera until set_camera() is called.
	for (int i = 0; i < 4; i++) {
		view_proj[i * 4 + i] = 1.0f;
	}
}

IkemenRuntime::~IkemenRuntime() {
	if (singleton == this) {
		singleton = nullptr;
	}
}

int IkemenRuntime::_fail(int p_code, const String &p_message) {
	error = p_message;
	return p_code;
}

bool IkemenRuntime::_gl_available() {
	OS *os = OS::get_singleton();
	// The names alone are not enough: --headless keeps the project's method/driver names but runs the dummy
	// rasterizer, where the GLES3 storage singletons do not exist.
	return os->get_current_rendering_method() == "gl_compatibility" && os->get_current_rendering_driver_name() == "opengl3" &&
			GLES3::TextureStorage::get_singleton() != nullptr;
}

String IkemenRuntime::_resolve_dll_path() const {
	String env_path = OS::get_singleton()->get_environment("IKEMEN_DLL");
	if (!env_path.is_empty()) {
		return env_path.replace_char('\\', '/');
	}
	return ProjectSettings::get_singleton()->globalize_path(IKEMEN_DLL_RES_PATH);
}

String IkemenRuntime::get_dll_path() const {
	return dll_file.is_empty() ? _resolve_dll_path() : dll_file;
}

int IkemenRuntime::_ensure_loaded() {
	if (loaded) {
		return OK;
	}
	if (library) {
		// Loaded but lacking exports: final for this process (a loaded Go runtime can never be unloaded/replaced).
		return IK_ERR_MISSING_SYMBOL;
	}
	// Not found / LoadLibrary failed: nothing is loaded, so a later start() may retry (e.g. after a DLL build).
	load_attempted = true;
	dll_file = _resolve_dll_path();

	if (!FileAccess::exists(dll_file)) {
		return _fail(IK_ERR_NO_DLL, vformat("Ikemen: ikemen.dll not found at \"%s\" (build it with ikemen/build/build_dll.cmd, or set IKEMEN_DLL).", dll_file));
	}

	OS *os = OS::get_singleton();
	// Keep the Go runtime (GC, loader goroutines) on 4 threads unless the caller chose otherwise.
	if (!os->has_environment("GOMAXPROCS")) {
		os->set_environment("GOMAXPROCS", "4");
	}
	// The DLL folder supplies SDL2 and friends. also_set_library_path covers the load itself; the PATH entry
	// covers libraries the DLL loads by name later (process-local, nothing persistent).
	String dll_dir = dll_file.get_base_dir().replace_char('/', '\\');
	String path_var = os->get_environment("PATH");
	if (!path_var.begins_with(dll_dir + ";")) {
		os->set_environment("PATH", dll_dir + ";" + path_var);
	}

	OS::GDExtensionData data;
	data.also_set_library_path = true;
	void *handle = nullptr;
	Error err = os->open_dynamic_library(dll_file, handle, &data);
	if (err != OK || handle == nullptr) {
		return _fail(IK_ERR_NO_DLL, vformat("Ikemen: LoadLibrary failed for \"%s\" (error %d): a dependency (SDL2.dll, ...) is probably missing beside it.", dll_file, (int)err));
	}
	library = handle;

	Vector<String> missing;
	auto bind = [&](const char *p_name, auto &r_fn) {
		void *sym = nullptr;
		if (os->get_dynamic_library_symbol_handle(library, p_name, sym, true) != OK || sym == nullptr) {
			missing.push_back(p_name);
			return;
		}
		r_fn = reinterpret_cast<std::remove_reference_t<decltype(r_fn)>>(sym);
	};
	bind("ikemen_init", api.init);
	bind("ikemen_gl_init", api.gl_init);
	bind("ikemen_gl_pump", api.gl_pump);
	// Optional for old DLLs (the idle pump then drains everything with ikemen_gl_pump, and only while the hook is set).
	void *pump_budget_symbol = nullptr;
	if (os->get_dynamic_library_symbol_handle(library, "ikemen_gl_pump_budget", pump_budget_symbol, true) == OK) {
		api.gl_pump_budget = reinterpret_cast<decltype(api.gl_pump_budget)>(pump_budget_symbol);
	}
	bind("ikemen_step", api.step);
	// Optional for old DLLs; start(pipeline=true) requires both symbols.
	void *begin_symbol = nullptr;
	void *end_symbol = nullptr;
	if (os->get_dynamic_library_symbol_handle(library, "ikemen_step_begin", begin_symbol, true) == OK) {
		api.step_begin = reinterpret_cast<decltype(api.step_begin)>(begin_symbol);
	}
	if (os->get_dynamic_library_symbol_handle(library, "ikemen_step_end", end_symbol, true) == OK) {
		api.step_end = reinterpret_cast<decltype(api.step_end)>(end_symbol);
	}
	bind("ikemen_render", api.render);
	// Optional for older DLLs (the DLL in this repository does not export it).
	void *scene_symbol = nullptr;
	if (os->get_dynamic_library_symbol_handle(library, "ikemen_render_scene", scene_symbol, true) == OK) {
		api.render_scene = reinterpret_cast<decltype(api.render_scene)>(scene_symbol);
	}
	bind("ikemen_command", api.command);
	bind("ikemen_state", api.state);
	bind("ikemen_events", api.events);
	bind("ikemen_last_error", api.last_error);
	bind("ikemen_status", api.status);
	bind("ikemen_shutdown", api.shutdown);
	// Optional for phase-1 SDL DLLs; required when start(config.audio=host).
	void *mix_symbol = nullptr;
	if (os->get_dynamic_library_symbol_handle(library, "ikemen_mix_audio", mix_symbol, true) == OK) {
		api.mix_audio = reinterpret_cast<decltype(api.mix_audio)>(mix_symbol);
	}

	if (!missing.is_empty()) {
		api = Api();
		return _fail(IK_ERR_MISSING_SYMBOL, vformat("Ikemen: \"%s\" lacks the exports %s (an old or wrong ikemen.dll?).", dll_file, String(", ").join(missing)));
	}
	loaded = true;
	print_verbose(vformat("Ikemen: loaded %s", dll_file));
	return OK;
}

int IkemenRuntime::_check_started() {
	if (!loaded) {
		if (!load_attempted) {
			return _fail(IK_ERR_NOT_STARTED, "Ikemen: start() has not been called.");
		}
		return library ? IK_ERR_MISSING_SYMBOL : IK_ERR_NO_DLL; // error already says why
	}
	if (!started) {
		return _fail(IK_ERR_NOT_STARTED, "Ikemen: start() has not succeeded.");
	}
	return OK;
}

int IkemenRuntime::_dll_result(int p_result, const char *p_what) {
	if (p_result < 0) {
		const char *msg = api.last_error ? api.last_error() : nullptr;
		error = vformat("Ikemen: %s returned %d: %s", p_what, p_result, (msg && msg[0]) ? String::utf8(msg) : String("(no message)"));
	}
	return p_result;
}

int IkemenRuntime::start(const String &p_ikemen_root, const Dictionary &p_config) {
	MutexLock lock(dll_mutex);
	if (started) {
		return _fail(IK_ERR_ALREADY_STARTED, "Ikemen: already started (one Ikemen world per process).");
	}
	int err = _ensure_loaded();
	if (err != OK) {
		return err;
	}

	String root = p_ikemen_root.is_empty() ? String(IKEMEN_ROOT_RES_PATH) : p_ikemen_root;
	if (root.begins_with("res://") || root.begins_with("user://")) {
		root = ProjectSettings::get_singleton()->globalize_path(root);
	}
	Dictionary config = p_config.duplicate(true);
	pipeline_enabled = bool(config.get("pipeline", false));
	if (pipeline_enabled && (!api.step_begin || !api.step_end)) {
		return _fail(IK_ERR_MISSING_SYMBOL, "Ikemen: pipeline=true requires ikemen_step_begin and ikemen_step_end.");
	}
	const bool host_audio = String(config.get("audio", "sdl")) == "host";
	if (host_audio) {
		if (!api.mix_audio) {
			return _fail(IK_ERR_MISSING_SYMBOL, "Ikemen: audio=host requires ikemen_mix_audio.");
		}
		// The negotiated driver rate, before Ikemen creates any sound resamplers.
		config["mix_rate"] = int(AudioServer::get_singleton()->get_mix_rate());
	}
	String config_json = JSON::stringify(config, "", false, true);
	int rc = api.init(root.utf8().get_data(), config_json.utf8().get_data());
	if (rc < 0) {
		return _dll_result(rc, "ikemen_init");
	}
	started = true;
	AudioStreamIkemen::set_mix_callback(host_audio ? api.mix_audio : nullptr);
	return rc;
}

int IkemenRuntime::status() {
	MutexLock lock(dll_mutex);
	int err = _check_started();
	if (err != OK) {
		return err;
	}
	return _dll_result(api.status(), "ikemen_status");
}

int IkemenRuntime::command(const Dictionary &p_command) {
	MutexLock lock(dll_mutex);
	int err = _check_started();
	if (err != OK) {
		return err;
	}
	if (!p_command.has("op")) {
		return _fail(IK_ERR_BAD_ARGUMENT, "Ikemen: command() needs an \"op\" key.");
	}
	String js = JSON::stringify(p_command, "", false, true);
	return _dll_result(api.command(js.utf8().get_data()), "ikemen_command");
}

int IkemenRuntime::step() {
	MutexLock lock(dll_mutex);
	int err = _check_started();
	if (err != OK) {
		return err;
	}
	return _dll_result(api.step(), "ikemen_step");
}

int IkemenRuntime::step_begin() {
	MutexLock lock(dll_mutex);
	int err = _check_started();
	if (err != OK) {
		return err;
	}
	if (!pipeline_enabled) {
		return _fail(IK_ERR_BAD_ARGUMENT, "Ikemen: step_begin requires pipeline=true at start().");
	}
	return _dll_result(api.step_begin(), "ikemen_step_begin");
}

int IkemenRuntime::step_end() {
	MutexLock lock(dll_mutex);
	int err = _check_started();
	if (err != OK) {
		return err;
	}
	return pipeline_enabled ? _dll_result(api.step_end(), "ikemen_step_end") : 0;
}

PackedFloat32Array IkemenRuntime::get_state() {
	PackedFloat32Array out;
	MutexLock lock(dll_mutex);
	if (_check_started() != OK) {
		return out;
	}
	out.resize(REC_FLOATS * MAX_FIGHTERS);
	int n = _dll_result(api.state(out.ptrw(), (int)out.size()), "ikemen_state");
	out.resize(CLAMP(n, 0, (int)out.size()));
	return out;
}

Array IkemenRuntime::poll_events() {
	Array out;
	MutexLock lock(dll_mutex);
	if (_check_started() != OK) {
		return out;
	}
	Vector<char> buf;
	buf.resize(EVENT_CHUNK_BYTES);
	String text;
	for (int chunk = 0; chunk < EVENT_MAX_CHUNKS; chunk++) {
		int n = _dll_result(api.events(buf.ptrw(), (int)buf.size()), "ikemen_events");
		if (n <= 0) {
			break;
		}
		n = MIN(n, (int)buf.size());
		text += String::utf8(buf.ptr(), n);
		if (n < buf.size()) {
			break; // drained
		}
	}
	if (text.is_empty()) {
		return out;
	}
	Ref<JSON> json;
	json.instantiate();
	Vector<String> lines = text.split("\n", false);
	for (const String &raw : lines) {
		String line = raw.strip_edges();
		if (line.is_empty()) {
			continue;
		}
		if (json->parse(line) == OK && json->get_data().get_type() == Variant::DICTIONARY) {
			out.push_back(json->get_data());
		} else {
			Dictionary bad;
			bad["e"] = "bad_event";
			bad["line"] = line;
			out.push_back(bad);
		}
	}
	return out;
}

int IkemenRuntime::set_overlay(RID p_viewport, int p_first_hud_layer, int p_flags) {
	if (!p_viewport.is_valid()) {
		return _fail(IK_ERR_BAD_ARGUMENT, "Ikemen: set_overlay() needs a valid viewport RID (get_tree().root.get_viewport_rid()).");
	}
	if (!_gl_available()) {
		return _fail(IK_ERR_NO_GL, vformat("Ikemen: the overlay needs the Compatibility renderer on OpenGL with a window (running \"%s\" / \"%s\"%s).", OS::get_singleton()->get_current_rendering_method(), OS::get_singleton()->get_current_rendering_driver_name(), GLES3::TextureStorage::get_singleton() ? "" : ", no GLES3 rasterizer: headless?"));
	}
	skip_guard = OS::get_singleton()->get_environment("IKEMEN_NO_GUARD") == "1";
	if (skip_guard) {
		WARN_PRINT("Ikemen: IKEMEN_NO_GUARD=1 - GL state guard disabled (diagnostics only).");
	}
	RenderingServer::get_singleton()->call_on_render_thread(callable_mp(this, &IkemenRuntime::_install_hook_rt).bind(p_viewport, p_first_hud_layer, p_flags));
	// Every main-loop iteration, drawn or not (a minimized window skips RenderingServer::draw, so the hook with it).
	SceneTree *tree = SceneTree::get_singleton();
	if (tree && !frame_connected) {
		tree->connect(SNAME("process_frame"), callable_mp(this, &IkemenRuntime::_on_process_frame));
		frame_connected = true;
	}
	return OK;
}

void IkemenRuntime::_on_process_frame() {
	if (started && RenderingServer::get_singleton()) {
		RenderingServer::get_singleton()->call_on_render_thread(callable_mp(this, &IkemenRuntime::_idle_pump_rt));
	}
}

// Render thread, once per main-loop iteration (queued before that iteration's draw). Frames the hook ran on are left
// alone (it pumped); after IDLE_AFTER_FRAMES frames in a row without it, the queued GL work is drained here, at most
// IDLE_PUMP_USEC per frame, under the state guard - which is skipped while the queue is empty.
void IkemenRuntime::_idle_pump_rt() {
	if (hook_runs != hook_runs_seen) {
		hook_runs_seen = hook_runs;
		idle_frames = 0;
		return;
	}
	if (++idle_frames < IDLE_AFTER_FRAMES) {
		return;
	}
	MutexLock lock(dll_mutex);
	if (!loaded || !started || gl_failed || !_gl_available()) {
		return;
	}
	if (api.gl_pump_budget ? api.gl_pump_budget(-1) == 0 : !hook_installed) {
		idle_backlog = 0;
		return; // nothing queued (no GL call at all)
	}
	uint64_t t0 = OS::get_singleton()->get_ticks_usec();
	for (int i = 0; i < 64 && glGetError() != GL_NO_ERROR; i++) {
	}
	{
		IkemenGLStateGuard guard; // (always: IKEMEN_NO_GUARD is the hook's diagnostic only)
		if (_ensure_gl_init()) {
			if (api.gl_pump_budget) {
				idle_backlog = api.gl_pump_budget(IDLE_PUMP_USEC);
			} else {
				api.gl_pump();
				idle_backlog = 0;
			}
			idle_pumps++;
		}
	}
	int errors = 0;
	while (glGetError() != GL_NO_ERROR && errors < 64) {
		errors++;
	}
	total_gl_errors += errors;
	idle_pump_usec_max = MAX(idle_pump_usec_max, OS::get_singleton()->get_ticks_usec() - t0);
}

bool IkemenRuntime::_ensure_gl_init() {
	if (!gl_inited) {
		gl_inited = true;
		gl_init_result = _dll_result(api.gl_init(), "ikemen_gl_init");
		if (gl_init_result < 0) {
			gl_failed = true;
			ERR_PRINT(error);
		}
	}
	return !gl_failed;
}

void IkemenRuntime::clear_overlay() {
	if (RenderingServer::get_singleton()) {
		RenderingServer::get_singleton()->call_on_render_thread(callable_mp(this, &IkemenRuntime::_remove_hook_rt));
	} else {
		_remove_hook_rt();
	}
}

void IkemenRuntime::_install_hook_rt(RID p_viewport, int p_first_layer, int p_flags) {
	overlay_viewport = p_viewport;
	overlay_first_layer = p_first_layer;
	overlay_flags = p_flags;
	RendererViewport::overlay_userdata = this;
	RendererViewport::overlay_viewport = p_viewport;
	RendererViewport::overlay_first_layer = p_first_layer;
	RendererViewport::overlay_hook = &IkemenRuntime::_overlay_hook;
	// The scene hook, for the same viewport's render target (refreshed by every overlay draw), with Godot's transparent
	// pass drawn after Ikemen's frame (env IKEMEN_LATE=0: drawn as before, Ikemen over all of Godot's 3D).
	RasterizerSceneGLES3::scene_hook_userdata = this;
	RasterizerSceneGLES3::scene_hook_render_target = RSG::viewport ? RSG::viewport->viewport_get_render_target(p_viewport) : RID();
	RasterizerSceneGLES3::scene_hook_late_transparent = OS::get_singleton()->get_environment("IKEMEN_LATE") != "0";
	RasterizerSceneGLES3::scene_hook = &IkemenRuntime::_scene_hook;
	scene_ran = false;
	late_drawn = false;
	hook_installed = true;
}

void IkemenRuntime::_remove_hook_rt() {
	if (RendererViewport::overlay_userdata == this) {
		RendererViewport::overlay_hook = nullptr;
		RendererViewport::overlay_userdata = nullptr;
		RendererViewport::overlay_viewport = RID();
	}
	if (RasterizerSceneGLES3::scene_hook_userdata == this) {
		RasterizerSceneGLES3::scene_hook = nullptr;
		RasterizerSceneGLES3::scene_hook_userdata = nullptr;
		RasterizerSceneGLES3::scene_hook_render_target = RID();
		RasterizerSceneGLES3::scene_hook_late_transparent = false;
	}
	scene_ran = false;
	late_drawn = false;
	hook_installed = false;
}

void IkemenRuntime::set_camera(const Projection &p_view_proj, const PackedFloat32Array &p_params) {
	MutexLock lock(dll_mutex);
	// Column-major, exactly as Projection stores it (columns[c][r] -> [c * 4 + r]).
	for (int c = 0; c < 4; c++) {
		for (int r = 0; r < 4; r++) {
			view_proj[c * 4 + r] = (float)p_view_proj.columns[c][r];
		}
	}
	for (int i = 0; i < 16; i++) {
		params[i] = i < p_params.size() ? p_params[i] : 0.0f;
	}
}

String IkemenRuntime::last_error() const {
	return error;
}

Dictionary IkemenRuntime::get_render_stats() const {
	Dictionary d;
	d["dll_path"] = dll_file;
	d["loaded"] = loaded;
	d["started"] = started;
	d["hook_installed"] = hook_installed;
	d["gl_available"] = _gl_available();
	d["gl_inited"] = gl_inited;
	d["gl_init_result"] = gl_init_result;
	d["frames_drawn"] = (int64_t)frames_drawn;
	d["last_render_result"] = last_render_result;
	d["last_render_usec"] = (int64_t)last_render_usec;
	d["last_gl_errors"] = last_gl_errors;
	d["total_gl_errors"] = (int64_t)total_gl_errors;
	d["fbo"] = last_fbo;
	d["size"] = Vector2i(last_size);
	d["idle_pumps"] = (int64_t)idle_pumps;
	d["idle_backlog"] = idle_backlog;
	d["idle_pump_usec_max"] = (int64_t)idle_pump_usec_max;
	d["first_hud_layer"] = overlay_first_layer;
	d["flags"] = overlay_flags | FLIP_Y | NO_CLEAR;
	d["scene_hook"] = api.render_scene != nullptr;
	d["scene_runs"] = (int64_t)scene_runs;
	d["last_scene_result"] = last_scene_result;
	d["last_scene_usec"] = (int64_t)last_scene_usec;
	return d;
}

void IkemenRuntime::shutdown() {
	// Atomic withdrawal; an in-flight callback remains safe because the Go DLL
	// stays loaded and HostSpeaker.Close serializes mixer teardown.
	AudioStreamIkemen::set_mix_callback(nullptr);
	// Called from the main thread at exit (Safe thread model: also the render thread).
	_remove_hook_rt();
	MutexLock lock(dll_mutex);
	if (loaded && started) {
		api.shutdown();
	}
	started = false;
}

void IkemenRuntime::_overlay_hook(RID p_viewport, RID p_render_target, void *p_userdata) {
	IkemenRuntime *self = static_cast<IkemenRuntime *>(p_userdata);
	if (self) {
		self->_draw_overlay(p_viewport, p_render_target);
	}
}

void IkemenRuntime::_draw_overlay(RID p_viewport, RID p_render_target) {
	hook_runs++; // (the idle pump leaves frames the hook ran on alone)
	if (RasterizerSceneGLES3::scene_hook_userdata == this) {
		RasterizerSceneGLES3::scene_hook_render_target = p_render_target;
	}
	// (drawn already, inside this frame's 3D pass before its transparent objects: _draw_late)
	if (late_drawn) {
		late_drawn = false;
		scene_ran = false;
		return;
	}
	GLES3::TextureStorage *ts = GLES3::TextureStorage::get_singleton();
	if (ts == nullptr) {
		return;
	}
	_draw_ikemen(ts->render_target_get_fbo(p_render_target), ts->render_target_get_size(p_render_target), 0, skip_guard);
}

// Ikemen's frame onto the render target's framebuffer `p_fbo` (`p_size`): from the overlay hook, over everything Godot
// drew in 3D; or from the scene hook's transparent phase (`p_extra_flags` LATE_PASS), before Godot's transparent pass.
void IkemenRuntime::_draw_ikemen(GLuint p_fbo, Size2i p_size, int p_extra_flags, bool p_no_guard) {
	// (this frame's 3D pass ran the scene hook's opaque phase: told to this draw only)
	const int scene_flag = scene_ran ? SCENE_FRESH : 0;
	scene_ran = false;
	MutexLock lock(dll_mutex);
	if (!loaded || !started || gl_failed || !_gl_available()) {
		return;
	}
	if (p_fbo == 0 || p_size.x <= 0 || p_size.y <= 0) {
		return;
	}
	last_fbo = (int)p_fbo;
	last_size = p_size;

	uint64_t t0 = OS::get_singleton()->get_ticks_usec();
	// Errors Godot left pending are not Ikemen's: clear them first.
	for (int i = 0; i < 64 && glGetError() != GL_NO_ERROR; i++) {
	}
	auto draw = [&]() {
		if (_ensure_gl_init()) {
			api.gl_pump();
			glBindFramebuffer(GL_FRAMEBUFFER, p_fbo);
			glViewport(0, 0, p_size.x, p_size.y);
			// Godot render targets are stored bottom-up (flipped on the final blit), so FLIP_Y is always set;
			// NO_CLEAR because Ikemen draws onto the scene's pixels.
			int flags = overlay_flags | FLIP_Y | NO_CLEAR | scene_flag | p_extra_flags;
			last_render_result = _dll_result(api.render(p_fbo, p_size.x, p_size.y, flags, view_proj, params), "ikemen_render");
			frames_drawn++;
		}
	};
	if (p_no_guard) {
		draw(); // diagnostic only (IKEMEN_NO_GUARD=1): shows what the foreign draw does to Godot's state
	} else {
		IkemenGLStateGuard guard;
		draw();
	}
	// Drain GL errors raised by the foreign draw so Godot never misattributes them.
	int errors = 0;
	while (glGetError() != GL_NO_ERROR && errors < 64) {
		errors++;
	}
	last_gl_errors = errors;
	total_gl_errors += errors;
	last_render_usec = OS::get_singleton()->get_ticks_usec() - t0;
}

void IkemenRuntime::_scene_hook(RID p_render_target, uint32_t p_fbo, int p_width, int p_height, bool p_post, int p_phase, void *p_userdata) {
	IkemenRuntime *self = static_cast<IkemenRuntime *>(p_userdata);
	if (!self) {
		return;
	}
	if (p_phase == RasterizerSceneGLES3::SCENE_HOOK_TRANSPARENT) {
		self->_draw_late(p_fbo, Size2i(p_width, p_height));
	} else {
		self->_draw_scene(p_render_target, p_fbo, p_width, p_height, p_post);
	}
}

// Render thread, inside Godot's 3D pass of the overlay's viewport, its transparent pass drawn late (after
// post-processing, on the render target at full size): Ikemen's whole frame is drawn here, right before that pass, so
// Godot's fire, smoke and shadows are blended over it where they are in front - the DLL leaves the depth of its solid
// sprites for that (LATE_PASS). Always under the state guard: Godot's pass goes on after this. The overlay hook of the
// frame then draws nothing.
void IkemenRuntime::_draw_late(GLuint p_fbo, Size2i p_size) {
	hook_runs++;
	_draw_ikemen(p_fbo, p_size, LATE_PASS, false);
	late_drawn = true;
}

// Render thread, inside Godot's 3D pass of the overlay's viewport (its opaque objects and sky drawn, its transparent
// ones next): the DLL works out what its frame needs from the 3D image there - how much of each pixel
// shows the floor itself (ikemen_render_scene). Always under the state guard - Godot's pass goes on after this, trusting
// the GL state it left.
void IkemenRuntime::_draw_scene(RID p_render_target, uint32_t p_fbo, int p_width, int p_height, bool p_post) {
	scene_ran = false;
	MutexLock lock(dll_mutex);
	if (!api.render_scene || !loaded || !started || !gl_inited || gl_failed || !_gl_available() || p_fbo == 0 || p_width <= 0 || p_height <= 0) {
		return;
	}
	GLES3::TextureStorage *ts = GLES3::TextureStorage::get_singleton();
	Size2i target = ts ? ts->render_target_get_size(p_render_target) : Size2i();
	if (target.x <= 0 || target.y <= 0) {
		return;
	}
	int flags = overlay_flags | FLIP_Y | NO_CLEAR | (p_post ? SCENE_POST : 0);
	// Asked first, without a GL call: a frame with nothing to draw there costs no state guard.
	if (api.render_scene(p_fbo, p_width, p_height, target.x, target.y, flags | SCENE_QUERY, view_proj, params) <= 0) {
		return;
	}
	uint64_t t0 = OS::get_singleton()->get_ticks_usec();
	for (int i = 0; i < 64 && glGetError() != GL_NO_ERROR; i++) {
	}
	{
		IkemenGLStateGuard guard;
		last_scene_result = _dll_result(api.render_scene(p_fbo, p_width, p_height, target.x, target.y, flags, view_proj, params), "ikemen_render_scene");
		scene_ran = last_scene_result > 0;
		scene_runs++;
	}
	int errors = 0;
	while (glGetError() != GL_NO_ERROR && errors < 64) {
		errors++;
	}
	total_gl_errors += errors;
	last_scene_usec = OS::get_singleton()->get_ticks_usec() - t0;
}

void IkemenRuntime::_bind_methods() {
	ClassDB::bind_method(D_METHOD("start", "ikemen_root", "config"), &IkemenRuntime::start);
	ClassDB::bind_method(D_METHOD("status"), &IkemenRuntime::status);
	ClassDB::bind_method(D_METHOD("command", "cmd"), &IkemenRuntime::command);
	ClassDB::bind_method(D_METHOD("step"), &IkemenRuntime::step);
	ClassDB::bind_method(D_METHOD("step_begin"), &IkemenRuntime::step_begin);
	ClassDB::bind_method(D_METHOD("step_end"), &IkemenRuntime::step_end);
	ClassDB::bind_method(D_METHOD("is_pipeline_enabled"), &IkemenRuntime::is_pipeline_enabled);
	ClassDB::bind_method(D_METHOD("get_state"), &IkemenRuntime::get_state);
	ClassDB::bind_method(D_METHOD("poll_events"), &IkemenRuntime::poll_events);
	ClassDB::bind_method(D_METHOD("set_overlay", "viewport_rid", "first_hud_layer", "flags"), &IkemenRuntime::set_overlay, DEFVAL(0));
	ClassDB::bind_method(D_METHOD("clear_overlay"), &IkemenRuntime::clear_overlay);
	ClassDB::bind_method(D_METHOD("set_camera", "view_proj", "params"), &IkemenRuntime::set_camera);
	ClassDB::bind_method(D_METHOD("last_error"), &IkemenRuntime::last_error);
	ClassDB::bind_method(D_METHOD("is_loaded"), &IkemenRuntime::is_loaded);
	ClassDB::bind_method(D_METHOD("is_started"), &IkemenRuntime::is_started);
	ClassDB::bind_method(D_METHOD("get_dll_path"), &IkemenRuntime::get_dll_path);
	ClassDB::bind_method(D_METHOD("get_render_stats"), &IkemenRuntime::get_render_stats);
	ClassDB::bind_method(D_METHOD("shutdown"), &IkemenRuntime::shutdown);

	BIND_CONSTANT(FLIP_Y);
	BIND_CONSTANT(NO_CLEAR);
	BIND_CONSTANT(DEPTH_TEST);
	BIND_CONSTANT(DEBUG_CLSN);
	BIND_CONSTANT(REC_FLOATS);
	BIND_CONSTANT(MAX_FIGHTERS);
	BIND_CONSTANT(IK_ERR_NO_DLL);
	BIND_CONSTANT(IK_ERR_MISSING_SYMBOL);
	BIND_CONSTANT(IK_ERR_NOT_STARTED);
	BIND_CONSTANT(IK_ERR_ALREADY_STARTED);
	BIND_CONSTANT(IK_ERR_NO_GL);
	BIND_CONSTANT(IK_ERR_BAD_ARGUMENT);
	BIND_CONSTANT(IK_ERR_DLL_CALL);
}
