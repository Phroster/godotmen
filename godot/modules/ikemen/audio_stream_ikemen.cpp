// Godotmen: no SDL device, generator queue, or secondary resampler.
#include "audio_stream_ikemen.h"

#include "core/object/class_db.h"

std::atomic<AudioStreamIkemen::MixAudio> AudioStreamIkemen::mix_callback{ nullptr };
std::atomic<AudioStreamPlaybackIkemen *> AudioStreamIkemen::consumer{ nullptr };

void AudioStreamIkemen::set_mix_callback(MixAudio p_callback) {
	mix_callback.store(p_callback, std::memory_order_release);
}

Ref<AudioStreamPlayback> AudioStreamIkemen::instantiate_playback() {
	Ref<AudioStreamPlaybackIkemen> playback;
	playback.instantiate();
	playback->stream = Ref<AudioStreamIkemen>(this);
	return playback;
}

String AudioStreamIkemen::get_stream_name() const {
	return "Ikemen GO (live)";
}

double AudioStreamIkemen::get_length() const {
	return 0.0; // Endless live stream.
}

bool AudioStreamIkemen::is_monophonic() const {
	return true;
}

void AudioStreamPlaybackIkemen::start(double p_from_pos) {
	// One Ikemen world has exactly one consumption cursor. A second player must
	// not consume the next half of the audio (or double the playback speed).
	AudioStreamPlaybackIkemen *expected = nullptr;
	if (!AudioStreamIkemen::consumer.compare_exchange_strong(expected, this, std::memory_order_acq_rel) && expected != this) {
		return;
	}
	mixed_frames.store(0, std::memory_order_relaxed);
	silent_frames.store(0, std::memory_order_relaxed);
	failed_pulls.store(0, std::memory_order_relaxed);
	active.store(true, std::memory_order_release);
}

void AudioStreamPlaybackIkemen::stop() {
	active.store(false, std::memory_order_release);
	AudioStreamPlaybackIkemen *expected = this;
	AudioStreamIkemen::consumer.compare_exchange_strong(expected, nullptr, std::memory_order_acq_rel);
}

bool AudioStreamPlaybackIkemen::is_playing() const {
	return active.load(std::memory_order_acquire);
}

int AudioStreamPlaybackIkemen::get_loop_count() const {
	return 0;
}

double AudioStreamPlaybackIkemen::get_playback_position() const {
	return double(mixed_frames.load(std::memory_order_relaxed)) / mix_rate.load(std::memory_order_relaxed);
}

void AudioStreamPlaybackIkemen::seek(double p_time) {
	// Live mixer: seeking or replaying cached PCM is unsupported.
}

void AudioStreamPlaybackIkemen::tag_used_streams() {
	if (stream.is_valid()) {
		stream->tag_used(get_playback_position());
	}
}

int AudioStreamPlaybackIkemen::mix(AudioFrame *p_buffer, float p_rate_scale, int p_frames) {
	if (!p_buffer || p_frames <= 0) {
		return 0;
	}
	for (int i = 0; i < p_frames; i++) {
		p_buffer[i] = AudioFrame(0.0f, 0.0f);
	}
	if (!active.load(std::memory_order_acquire)) {
		return 0;
	}

	const int rate = int(AudioServer::get_singleton()->get_mix_rate());
	mix_rate.store(rate, std::memory_order_relaxed);
	// Fixed live clock: pitch_scale/global playback speed must stay at 1.0.
	// p_rate_scale is intentionally ignored; the native mixer runs at rate.
	constexpr int QUANTUM = 512; // bounds lock holds and stack storage.
	float samples[QUANTUM * 2];
	for (int offset = 0; offset < p_frames; offset += QUANTUM) {
		const int count = MIN(QUANTUM, p_frames - offset);
		AudioStreamIkemen::MixAudio callback = AudioStreamIkemen::mix_callback.load(std::memory_order_acquire);
		if (!callback || !active.load(std::memory_order_acquire)) {
			silent_frames.fetch_add(count, std::memory_order_relaxed);
			continue;
		}
		// Defend against an absent/short/failing DLL as well as Go's own clearing.
		for (int i = 0; i < count * 2; i++) {
			samples[i] = 0.0f;
		}
		int got = callback(samples, count, rate);
		if (got < 0 || got > count) {
			failed_pulls.fetch_add(1, std::memory_order_relaxed);
			got = 0;
		}
		for (int i = 0; i < got; i++) {
			p_buffer[offset + i] = AudioFrame(samples[i * 2], samples[i * 2 + 1]);
		}
		silent_frames.fetch_add(count - got, std::memory_order_relaxed);
	}
	mixed_frames.fetch_add(p_frames, std::memory_order_relaxed);
	// Silence is a successful live block. A short return would make AudioServer
	// retire the playback, preventing sound started later from ever being heard.
	return p_frames;
}

void AudioStreamPlaybackIkemen::_bind_methods() {
	ClassDB::bind_method(D_METHOD("get_silent_frames"), &AudioStreamPlaybackIkemen::get_silent_frames);
	ClassDB::bind_method(D_METHOD("get_failed_pulls"), &AudioStreamPlaybackIkemen::get_failed_pulls);
}

AudioStreamPlaybackIkemen::~AudioStreamPlaybackIkemen() {
	stop();
}
