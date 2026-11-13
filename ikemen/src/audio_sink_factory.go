//go:build !overlay

package main

// Retains the standalone SDL path after F applies the shared init hunk.
func newAudioSink() AudioSink { return &SDLSpeaker{} }

// Shared SoundEffect hooks retain the original SDL panning outside overlay.
func ovAudioTrackSound(sfx *SoundEffect)                        {}
func ovAudioForgetSound(sfx *SoundEffect)                       {}
func ovAudioPanGains(sfx *SoundEffect) (float32, float32, bool) { return 0, 0, false }

// (host_ov_frame.go is in every build: the stand-alone exe has no host audio - the core check builds again)
func ovAudioSnapshotPan()                               {}
func ovConfigureAudio(cfg map[string]interface{}) error { return nil }
