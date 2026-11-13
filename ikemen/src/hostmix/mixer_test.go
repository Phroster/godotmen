package hostmix

import (
	"math"
	"math/rand"
	"testing"

	"github.com/gopxl/beep/v2"
)

// tst: a deterministic test streamer; two made with the same seed behave the same call for call. Kinds:
//
//	0 finite source (random samples, exact and negative zeros; drains with a short read)
//	1 partial writer: writes only some of the samples it reports (the rest is whatever the buffer held - the mixer's
//	  block from the streamer before it, or the zeroes of the call's start)
//	2 adder: reads the buffer and adds to it (also sees what the buffer held)
//	3 quitter: full reads, then !ok at a random call
//	4 short: returns fewer samples than asked at random calls (drained, removed)
type tst struct {
	kind int
	rng  *rand.Rand
	left int
}

func newTst(kind int, seed int64) *tst {
	r := rand.New(rand.NewSource(seed))
	return &tst{kind: kind, rng: r, left: 1 + r.Intn(6000)}
}

func (s *tst) val() float64 {
	switch s.rng.Intn(8) {
	case 0:
		return 0
	case 1:
		return math.Copysign(0, -1)
	default:
		return s.rng.Float64()*2 - 1
	}
}

func (s *tst) Stream(samples [][2]float64) (int, bool) {
	switch s.kind {
	case 0:
		if s.left <= 0 {
			return 0, false
		}
		n := len(samples)
		if n > s.left {
			n = s.left
		}
		for i := 0; i < n; i++ {
			samples[i] = [2]float64{s.val(), s.val()}
		}
		s.left -= n
		return n, true
	case 1:
		w := s.rng.Intn(len(samples) + 1)
		for i := 0; i < w; i++ {
			samples[i][s.rng.Intn(2)] = s.val()
		}
		return len(samples), true
	case 2:
		for i := range samples {
			samples[i][0] += s.val() * 0.5
			samples[i][1] = samples[i][1]*0.75 + s.val()
		}
		return len(samples), true
	case 3:
		if s.rng.Intn(40) == 0 {
			return 0, false
		}
		for i := range samples {
			samples[i] = [2]float64{s.val(), s.val()}
		}
		return len(samples), true
	default:
		n := len(samples)
		if s.rng.Intn(30) == 0 {
			n = s.rng.Intn(n + 1)
		}
		for i := 0; i < n; i++ {
			samples[i] = [2]float64{s.val(), s.val()}
		}
		return n, true
	}
}
func (s *tst) Err() error { return nil }

// Every sample (bit for bit), count, ok and streamer count of beep's Mixer and this one, over random streamer mixes,
// streamers added between calls, nested mixers, both KeepAlive modes and block sizes across the 512-frame working block
func TestMixerBitIdentical(t *testing.T) {
	rng := rand.New(rand.NewSource(20261017))
	samples, calls := 0, 0
	for it := 0; it < 400; it++ {
		var a beep.Mixer
		var b Mixer
		keep := it%3 != 0
		a.KeepAlive(keep)
		b.KeepAlive(keep)
		// nested: a mixer inside the mixer (the game's speaker mixer streams the sounds' mixer through a Normalizer)
		var na beep.Mixer
		var nb Mixer
		nested := it%4 == 0
		if nested {
			keepN := rng.Intn(2) == 0
			na.KeepAlive(keepN)
			nb.KeepAlive(keepN)
			a.Add(&na)
			b.Add(&nb)
		}
		add := func() {
			kind, seed := rng.Intn(5), rng.Int63()
			if nested && rng.Intn(2) == 0 {
				na.Add(newTst(kind, seed))
				nb.Add(newTst(kind, seed))
				return
			}
			a.Add(newTst(kind, seed))
			b.Add(newTst(kind, seed))
		}
		for k := rng.Intn(8); k > 0; k-- {
			add()
		}
		bufA := make([][2]float64, 2048)
		bufB := make([][2]float64, 2048)
		for c := 0; c < 60; c++ {
			if rng.Intn(5) == 0 {
				add()
			}
			n := 1 + rng.Intn(1500)
			if rng.Intn(3) == 0 {
				n = 128 // (the host callback's block)
			}
			for i := range bufA[:n] { // (the same garbage in both outputs)
				v := [2]float64{rng.Float64(), rng.Float64()}
				bufA[i], bufB[i] = v, v
			}
			an, aok := a.Stream(bufA[:n])
			bn, bok := b.Stream(bufB[:n])
			calls++
			if an != bn || aok != bok || a.Len() != b.Len() || na.Len() != nb.Len() {
				t.Fatalf("it %d call %d: n/ok/len %d %v %d %d vs %d %v %d %d", it, c, an, aok, a.Len(), na.Len(),
					bn, bok, b.Len(), nb.Len())
			}
			for i := 0; i < n; i++ {
				for ch := 0; ch < 2; ch++ {
					if math.Float64bits(bufA[i][ch]) != math.Float64bits(bufB[i][ch]) {
						t.Fatalf("it %d call %d sample %d/%d: %v vs %v", it, c, i, ch, bufA[i][ch], bufB[i][ch])
					}
				}
			}
			samples += n
		}
		if rng.Intn(4) == 0 {
			a.Clear()
			b.Clear()
			if a.Len() != b.Len() {
				t.Fatal("Clear")
			}
		}
	}
	t.Logf("%d calls, %d frames bit-identical", calls, samples)
}

// The working block is not allocated per Stream call any more
func TestMixerStreamAllocs(t *testing.T) {
	var m Mixer
	m.Add(newTst(2, 1), newTst(2, 2))
	buf := make([][2]float64, 128)
	if n := testing.AllocsPerRun(100, func() { m.Stream(buf) }); n != 0 {
		t.Fatalf("%v allocations per Stream", n)
	}
}
