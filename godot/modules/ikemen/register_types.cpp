// Godotmen: Ikemen GO overlay module.
#include "register_types.h"

#include "ikemen_runtime.h"
#include "audio_stream_ikemen.h"

#include "core/config/engine.h"
#include "core/object/class_db.h"

static IkemenRuntime *ikemen_runtime = nullptr;

void initialize_ikemen_module(ModuleInitializationLevel p_level) {
	if (p_level != MODULE_INITIALIZATION_LEVEL_SCENE) {
		return;
	}
	GDREGISTER_CLASS(AudioStreamIkemen);
	GDREGISTER_CLASS(AudioStreamPlaybackIkemen);
	GDREGISTER_CLASS(IkemenRuntime);
	ikemen_runtime = memnew(IkemenRuntime);
	Engine::get_singleton()->add_singleton(Engine::Singleton("Ikemen", ikemen_runtime, "IkemenRuntime"));
}

void uninitialize_ikemen_module(ModuleInitializationLevel p_level) {
	if (p_level != MODULE_INITIALIZATION_LEVEL_SCENE) {
		return;
	}
	if (ikemen_runtime) {
		// Removes the render hook and shuts Ikemen down. The DLL itself is never unloaded (Go c-shared runtime).
		ikemen_runtime->shutdown();
		Engine::get_singleton()->remove_singleton("Ikemen");
		memdelete(ikemen_runtime);
		ikemen_runtime = nullptr;
	}
}
