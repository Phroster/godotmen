// Package hostmix is beep's Mixer (github.com/gopxl/beep/v2 mixer.go, the version in go.mod) with its 8 KB working
// block kept in the Mixer instead of a local array. beep's `var tmp [512][2]float64` escapes to
// the heap on every Stream call (it is handed to the streamers' interface method): in a 101-fighter fight the host's
// mix callback streams 3 mixers (the speaker's, the sounds' and the videos') per 128-frame block, ~9 MB a second of
// garbage for the Go GC. Here the block is zeroed at the start of every Stream call exactly as the fresh local array
// was, and everything else is beep's code unchanged, so every sample, count and removal is bit-identical
// (TestMixerBitIdentical). A Mixer is not safe for concurrent use - as beep's is not (its streamer list): callers
// hold the speaker lock - and a Mixer must not stream itself (it would recurse forever in beep too).
package hostmix

import "github.com/gopxl/beep/v2"

// Mixer allows for dynamic mixing of arbitrary number of Streamers. Mixer automatically removes
// drained Streamers. Depending on the KeepAlive() setting, Stream will either play silence or
// drain when all Streamers have been drained. By default, Mixer keeps playing silence.
type Mixer struct {
	streamers     []beep.Streamer
	stopWhenEmpty bool
	tmp           [512][2]float64 // (beep's per-call local array, reused; zeroed at each Stream call)
}

// KeepAlive configures the Mixer to either keep playing silence when all its Streamers have
// drained (keepAlive == true) or stop playing (keepAlive == false).
func (m *Mixer) KeepAlive(keepAlive bool) {
	m.stopWhenEmpty = !keepAlive
}

// Len returns the number of Streamers currently playing in the Mixer.
func (m *Mixer) Len() int {
	return len(m.streamers)
}

// Add adds Streamers to the Mixer.
func (m *Mixer) Add(s ...beep.Streamer) {
	m.streamers = append(m.streamers, s...)
}

// Clear removes all Streamers from the mixer.
func (m *Mixer) Clear() {
	for i := range m.streamers {
		m.streamers[i] = nil
	}
	m.streamers = m.streamers[:0]
}

// Stream the samples of all Streamers currently in the Mixer mixed together. Depending on the
// KeepAlive() setting, Stream will either play silence or drain when all Streamers have been
// drained.
func (m *Mixer) Stream(samples [][2]float64) (n int, ok bool) {
	if m.stopWhenEmpty && len(m.streamers) == 0 {
		return 0, false
	}

	// (beep declares `var tmp [512][2]float64` here - a zeroed array per call; the kept one is zeroed the same)
	tmp := &m.tmp
	*tmp = [512][2]float64{}

	for len(samples) > 0 {
		//toStream := min(len(tmp), len(samples))
		toStream := len(tmp)
		if toStream > len(samples) {
			toStream = len(samples)
		}

		// clear the samples
		//clear(samples[:toStream])
		for i := range samples[:toStream] {
			samples[i] = [2]float64{}
		}

		snMax := 0
		for si := 0; si < len(m.streamers); si++ {
			// mix the stream
			sn, sok := m.streamers[si].Stream(tmp[:toStream])
			for i := range tmp[:sn] {
				samples[i][0] += tmp[i][0]
				samples[i][1] += tmp[i][1]
			}
			if sn > snMax {
				snMax = sn
			}

			if sn < toStream || !sok {
				// remove drained streamer
				last := len(m.streamers) - 1
				m.streamers[si] = m.streamers[last]
				m.streamers[last] = nil
				m.streamers = m.streamers[:last]
				si--

				if m.stopWhenEmpty && len(m.streamers) == 0 {
					return n + snMax, true
				}
			}
		}

		samples = samples[toStream:]
		n += toStream
	}

	return n, true
}

// Err always returns nil for Mixer.
//
// There are two reasons. The first one is that erroring Streamers are immediately drained and
// removed from the Mixer. The second one is that one Streamer shouldn't break the whole Mixer and
// you should handle the errors right where they can happen.
func (m *Mixer) Err() error {
	return nil
}
