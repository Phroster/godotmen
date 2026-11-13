package hostrs

import (
	"math"
	"math/rand"
	"testing"

	"github.com/gopxl/beep/v2"
)

// src: a deterministic finite stereo streamer (random samples, some exact zeros, negative zeros and repeats)
type src struct {
	data [][2]float64
	pos  int
}

func (s *src) Stream(samples [][2]float64) (int, bool) {
	if s.pos >= len(s.data) {
		return 0, false
	}
	n := copy(samples, s.data[s.pos:])
	s.pos += n
	return n, true
}
func (s *src) Err() error { return nil }

func makeData(rng *rand.Rand, n int) [][2]float64 {
	d := make([][2]float64, n)
	for i := range d {
		for c := 0; c < 2; c++ {
			switch rng.Intn(10) {
			case 0:
				d[i][c] = 0
			case 1:
				d[i][c] = math.Copysign(0, -1)
			case 2:
				if i > 0 {
					d[i][c] = d[i-1][c]
				}
			default:
				d[i][c] = rng.Float64()*2 - 1
			}
		}
	}
	return d
}

// Every output sample, n and ok of the two resamplers, bit for bit, over random qualities, ratios, chunk sizes and
// SetRatio calls (the mixer streams 128-frame blocks; SetFreqMul calls SetRatio between them)
func TestResamplerBitIdentical(t *testing.T) {
	rng := rand.New(rand.NewSource(20261017))
	ratios := []float64{0.25, 11025.0 / 44100, 22050.0 / 44100, 1.0 / 3, 0.5000001, 1, 2.7, 22050.0 / 48000, 44100.0 / 48000, 1.7}
	samples, cases := 0, 0
	for it := 0; it < 1500; it++ {
		q := 1 + rng.Intn(16)
		if it%3 == 0 {
			q = 1 // (the game's AudioResampleQuality)
		}
		ratio := ratios[rng.Intn(len(ratios))]
		data := makeData(rng, 1+rng.Intn(5000))
		a := beep.ResampleRatio(q, ratio, &src{data: data})
		b := ResampleRatio(q, ratio, &src{data: data})
		for round := 0; ; round++ {
			if rng.Intn(20) == 0 {
				r2 := ratios[rng.Intn(len(ratios))] * (0.5 + rng.Float64())
				a.SetRatio(r2)
				b.SetRatio(r2)
			}
			k := 1 + rng.Intn(700)
			if rng.Intn(3) == 0 {
				k = 128
			}
			oa := make([][2]float64, k)
			ob := make([][2]float64, k)
			for i := range oa { // (stale contents beyond n must not matter: both start equal)
				oa[i] = [2]float64{float64(i), -float64(i)}
				ob[i] = oa[i]
			}
			na, oka := a.Stream(oa)
			nb, okb := b.Stream(ob)
			if na != nb || oka != okb {
				t.Fatalf("case %d q %d ratio %v round %d: n/ok %d/%v vs %d/%v", it, q, ratio, round, na, oka, nb, okb)
			}
			for i := range oa {
				for c := 0; c < 2; c++ {
					if math.Float64bits(oa[i][c]) != math.Float64bits(ob[i][c]) {
						t.Fatalf("case %d q %d ratio %v round %d sample %d ch %d: %v (%x) vs %v (%x)", it, q, ratio, round, i, c,
							oa[i][c], math.Float64bits(oa[i][c]), ob[i][c], math.Float64bits(ob[i][c]))
					}
				}
			}
			samples += na
			if a.Ratio() != b.Ratio() {
				t.Fatalf("ratio differs")
			}
			if !oka {
				break
			}
			if round > 100000 {
				t.Fatalf("no end")
			}
		}
		cases++
	}
	t.Logf("%d cases, %d output samples bit-identical", cases, samples)
}

func BenchmarkBeepQ1(b *testing.B) { bench(b, 1, true) }
func BenchmarkOursQ1(b *testing.B) { bench(b, 1, false) }
func BenchmarkBeepQ4(b *testing.B) { bench(b, 4, true) }
func BenchmarkOursQ4(b *testing.B) { bench(b, 4, false) }

func bench(b *testing.B, q int, beepOne bool) {
	rng := rand.New(rand.NewSource(1))
	data := makeData(rng, 1<<16)
	out := make([][2]float64, 128)
	var s beep.Streamer
	reset := func() {
		if beepOne {
			s = beep.ResampleRatio(q, 22050.0/44100, &src{data: data})
		} else {
			s = ResampleRatio(q, 22050.0/44100, &src{data: data})
		}
	}
	reset()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := s.Stream(out); !ok {
			reset()
		}
	}
}
