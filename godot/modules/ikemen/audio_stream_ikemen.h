// Godotmen: Ikemen's final audio mix, pulled by Godot's audio thread.
#pragma once

#include "servers/audio/audio_stream.h"

#include <atomic>

class AudioStreamPlaybackIkemen;

class AudioStreamIkemen : public AudioStream {
	GDCLASS(AudioStreamIkemen, AudioStream);
	friend class AudioStreamPlaybackIkemen;

public:
	using MixAudio = int (*)(float *, int, int); // ikemen_mix_audio; caller-owned L/R floats.

private:
	static std::atomic<MixAudio> mix_callback;
	static std::atomic<AudioStreamPlaybackIkemen *> consumer;

protected:
	static void _bind_methods() {}

public:
	// Runtime publishes after successful init, clears BEFORE ikemen_shutdown.
	// No AudioServer or DLL/world mutex: those locks must never nest here.
	// The Go DLL must remain loaded, including while an old callback finishes.
	static void set_mix_callback(MixAudio p_callback);

	Ref<AudioStreamPlayback> instantiate_playback() override;
	String get_stream_name() const override;
	double get_length() const override;
	bool is_monophonic() const override;
	bool can_be_sampled() const override { return false; }
};

// Deliberately derives directly from AudioStreamPlayback. Resampled's cubic
// interpolator would change PCM even with nominally equal rates and add latency.
class AudioStreamPlaybackIkemen : public AudioStreamPlayback {
	GDCLASS(AudioStreamPlaybackIkemen, AudioStreamPlayback);
	friend class AudioStreamIkemen;

	Ref<AudioStreamIkemen> stream;
	std::atomic<bool> active{ false };
	std::atomic<uint64_t> mixed_frames{ 0 };
	std::atomic<uint64_t> silent_frames{ 0 };
	std::atomic<uint64_t> failed_pulls{ 0 };
	std::atomic<int> mix_rate{ 44100 };

protected:
	static void _bind_methods();

public:
	void start(double p_from_pos = 0.0) override;
	void stop() override;
	bool is_playing() const override;
	int get_loop_count() const override;
	double get_playback_position() const override;
	void seek(double p_time) override;
	void tag_used_streams() override;
	int mix(AudioFrame *p_buffer, float p_rate_scale, int p_frames) override;

	uint64_t get_silent_frames() const { return silent_frames.load(std::memory_order_relaxed); }
	uint64_t get_failed_pulls() const { return failed_pulls.load(std::memory_order_relaxed); }

	~AudioStreamPlaybackIkemen() override;
};
