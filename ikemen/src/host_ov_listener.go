package main

// ovListenerX: the Ikemen world x that sounds are panned around (the host's sound panning, audio_host_pan.go). The
// host draws the fight with its own camera, so Ikemen's 2D camera - which every sound is panned against in stock
// Ikemen - says nothing about what is seen. The host names the fighter its view follows with op "listener"
// {"h": handle}: a sound near that fighter plays in both ears at its own volume, one off to a side toward that ear.
// With no listener set (or that fighter gone) ok is false and Ikemen's own panning is used.
func ovListenerX() (float32, bool) {
	if !ovFFA {
		return 0, false
	}
	pn := int(ovListenerH.Load())
	if pn < 0 || pn >= MaxSimul*2 || !ov.slots[pn].ready {
		return 0, false
	}
	if c := ovLive(pn); c != nil {
		return c.pos[0] * c.localscl, true
	}
	return 0, false
}
