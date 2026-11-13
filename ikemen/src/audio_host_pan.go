//go:build overlay

package main

import "sync"

// SoundEffect's SDL path reads sys.cam and the fighter's live x pointer from
// its mixing goroutine. The host path must not read those while logic writes
// them. Publish Ikemen's own panning inputs at its tick boundary instead. This
// is not a second panner: ovAudioPanGains uses SoundEffect.Stream's exact float32
// arithmetic, with a coherent set of the same Ikemen coordinates/configuration.
// Values are immutable after publication; no audio-side allocation or I/O.
type hostAudioPanState struct {
	owner            *HostSpeaker
	enabled          bool
	r, scale, offset float32
}

var hostAudioPanCache sync.Map // *SoundEffect -> hostAudioPanState

// Called with the speaker mutex held, before publishing a new SoundChannel
// into sys.soundMixer and whenever SetPan changes its parameters.
func ovAudioTrackSound(sfx *SoundEffect) {
	if s := hostAudioCurrent.Load(); s != nil {
		hostAudioPanCache.Store(sfx, snapshotHostAudioPan(s, sfx))
	}
}

func ovAudioForgetSound(sfx *SoundEffect) {
	if sfx != nil {
		hostAudioPanCache.Delete(sfx)
	}
}

func snapshotHostAudioPan(owner *HostSpeaker, sfx *SoundEffect) hostAudioPanState {
	p := hostAudioPanState{owner: owner}
	if !sys.cfg.Sound.StereoEffects || (sfx.x == nil && sfx.pan == 0) {
		return p
	}
	leftEdge := sys.cam.ScreenPos[0] + sys.cam.Offset[0]
	screenWidth := float32(sys.gameWidth) / sys.cam.Scale
	if x, ok := ovListenerX(); ok {
		leftEdge = x - screenWidth/2 // (the fight: around the player, whom the view follows - host_ov_listener.go)
	}
	rightEdge := leftEdge + screenWidth
	if sfx.x != nil {
		p.r = ((rightEdge - sfx.localscl**sfx.x) - sfx.pan) / screenWidth
	} else {
		p.r = ((rightEdge-leftEdge)/2 - sfx.pan) / screenWidth
	}
	p.enabled = true
	p.scale = sys.cfg.Sound.PanningRange / 100
	p.offset = (100 - sys.cfg.Sound.PanningRange) / 200
	return p
}

// F calls this on the Ikemen thread after action(), before publishing ovDone
// or first ready status. It also covers sounds whose owner moves after PlaySnd.
func ovAudioSnapshotPan() {
	s := hostAudioCurrent.Load()
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		return
	}
	hostAudioPanCache.Range(func(key, value interface{}) bool {
		if value.(hostAudioPanState).owner == s {
			sfx := key.(*SoundEffect)
			hostAudioPanCache.Store(sfx, snapshotHostAudioPan(s, sfx))
		}
		return true
	})
}

// Called by SoundEffect.Stream under the same speaker lock as Stream/SetVolume.
// It never dereferences the live owner position or reads the camera/config.
func ovAudioPanGains(sfx *SoundEffect) (float32, float32, bool) {
	value, ok := hostAudioPanCache.Load(sfx)
	if !ok {
		return 0, 0, false
	}
	p := value.(hostAudioPanState)
	if !p.enabled {
		return sfx.volume, sfx.volume, true
	}
	lv := Clamp(sfx.volume*2*(p.r*p.scale+p.offset), 0, 512)
	rv := Clamp(sfx.volume*2*((1-p.r)*p.scale+p.offset), 0, 512)
	return lv, rv, true
}

// Close calls this AFTER synchronizing any in-flight mix with its mutex.
func clearHostAudioPan(owner *HostSpeaker) {
	hostAudioPanCache.Range(func(key, value interface{}) bool {
		if value.(hostAudioPanState).owner == owner {
			hostAudioPanCache.Delete(key)
		}
		return true
	})
}
