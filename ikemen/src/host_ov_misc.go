package main

// Small pieces shared by the host layer's files.

import (
	"errors"
	"fmt"
)

// ovJSONInts: v as a JSON array
func ovJSONInts(v []int32) string {
	b := []byte{'['}
	for i, n := range v {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, []byte(fmt.Sprint(n))...)
	}
	return string(append(b, ']'))
}

// Parity mode (config "parity_mode"): a stock round - the two boot fighters, Ikemen's own round flow, flat 2D drawing,
// none of the host-only shortcuts - so a run can be compared with stock Ikemen GO. The fighters are the boot fighters
// of the init config; nothing is spawned later.

// ovParityConfigure: (init) nothing to prepare
func ovParityConfigure() error { return nil }

// ovParitySpawn: (op spawn in parity mode) refused - a parity run has its boot fighters only
func ovParitySpawn(m map[string]interface{}) (int, error) {
	return -1, errors.New("spawn is not available in parity_mode: the fighters are the init config's")
}

// ovParityBetweenTicks, ovParityShutdown: nothing to do
func ovParityBetweenTicks() {}
func ovParityShutdown()     {}

// Vestigial shims, each to be removed together with its last caller:
//   - ovTestOn: a scripted check that is not part of this build (read by ovHostInput in host_ov_core.go)
//   - ovSay: a draw diagnostic that is not part of this build (called in host_ov_core.go)
//   - ovSfxMix: a host-side mix hold on a sound effect, never set (SoundEffect.ovMix, read by host_ov_replay.go)
const ovTestOn = false

func ovSay(e *Explod, why string) {}

type ovSfxMix struct {
	managed bool
	pn      int16
}
