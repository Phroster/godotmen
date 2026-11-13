// Package hostrs is beep's Resampler (github.com/gopxl/beep/v2 resample.go at v2.1.1-0.20240921133731-defe79638e99,
// the version go.mod pins) with the same arithmetic, made cheaper for the host mixer.
//
// beep works out every output sample once per channel: it copies the window's points (position, that channel's
// value) and evaluates the Lagrange polynomial through them, whose weight for point j,
//
//	l_j = 1.0 * (x - X_0)/(X_j - X_0) * ... (every m != j, in order),
//
// depends only on the position x and the window's positions X - never on the channel. Here the weights are worked
// out once per output sample, with the very same expressions in the very same order, and each channel's value is then
// summed exactly as beep sums it: y = 0.0; y += Y_j * l_j for j in order (a product is the same either way round).
// Every float operation that produces a sample is the same operation on the same operands as in beep, so every
// output sample is bit-identical (TestResamplerBitIdentical compares the two on random input, chunk sizes, qualities,
// ratios and SetRatio calls). Buffering, the end of the stream, SetRatio and Err are beep's, line for line.
package hostrs

import (
	"fmt"
	"math"

	"github.com/gopxl/beep/v2"
)

const singleBufferSize = 512 // (beep's resamplerSingleBufferSize)

// Resample is beep.Resample.
func Resample(quality int, old, new beep.SampleRate, s beep.Streamer) *Resampler {
	return ResampleRatio(quality, float64(old)/float64(new), s)
}

// ResampleRatio is beep.ResampleRatio.
func ResampleRatio(quality int, ratio float64, s beep.Streamer) *Resampler {
	if quality < 1 || 64 < quality {
		panic(fmt.Errorf("resample: invalid quality: %d", quality))
	}
	if ratio <= 0 || math.IsInf(ratio, 0) || math.IsNaN(ratio) {
		panic(fmt.Errorf("resample: invalid ratio: %f", ratio))
	}
	return &Resampler{
		s:     s,
		ratio: ratio,
		buf1:  make([][2]float64, singleBufferSize),
		buf2:  make([][2]float64, singleBufferSize),
		xs:    make([]float64, quality*2),
		ws:    make([]float64, quality*2),
		ps:    make([]*[2]float64, quality*2),
		off:   -singleBufferSize,
		pos:   0.0,
		end:   math.MaxInt,
	}
}

// Resampler is beep.Resampler (see the package comment).
type Resampler struct {
	s          beep.Streamer
	ratio      float64
	buf1, buf2 [][2]float64
	xs, ws     []float64     // one output sample's window positions and Lagrange weights (beep's pts: quality*2)
	ps         []*[2]float64 // ...and its samples
	off        int
	pos        float64
	end        int
}

// Stream is beep's Resampler.Stream.
func (r *Resampler) Stream(samples [][2]float64) (n int, ok bool) {
	for len(samples) > 0 {
		wantPos := r.pos * r.ratio
		windowStart := int(wantPos) - (len(r.xs)-1)/2 // (inclusive)
		windowEnd := int(wantPos) + len(r.xs)/2 + 1   // (exclusive)
		for windowEnd > r.off+singleBufferSize {
			sn, _ := r.s.Stream(r.buf1)
			if sn < len(r.buf1) {
				r.end = r.off + singleBufferSize + sn
			}
			r.buf1, r.buf2 = r.buf2, r.buf1
			r.off += singleBufferSize
		}
		if int(wantPos) >= r.end {
			return n, n > 0
		}
		if windowStart < 0 {
			windowStart = 0
		}
		if windowEnd > r.end {
			windowEnd = r.end
		}
		numPts := windowEnd - windowStart
		offBuf1 := r.off - singleBufferSize
		if numPts == 2 { // (quality 1, the game's: beep's lagrange through two points, unrolled)
			x0, x1 := float64(windowStart), float64(windowStart+1)
			l0 := 1.0
			l0 *= (wantPos - x1) / (x0 - x1)
			l1 := 1.0
			l1 *= (wantPos - x0) / (x1 - x0)
			p0, p1 := r.at(windowStart, offBuf1), r.at(windowStart+1, offBuf1)
			for c := range samples[0] {
				y := 0.0
				y += p0[c] * l0
				y += p1[c] * l1
				samples[0][c] = y
			}
		} else {
			xs, ws, ps := r.xs[:numPts], r.ws[:numPts], r.ps[:numPts]
			for i := range xs {
				xs[i] = float64(windowStart + i) // (beep's point.X)
				ps[i] = r.at(windowStart+i, offBuf1)
			}
			for j := range ws { // (beep's lagrange: l for point j)
				l := 1.0
				for m := range xs {
					if j == m {
						continue
					}
					l *= (wantPos - xs[m]) / (xs[j] - xs[m])
				}
				ws[j] = l
			}
			for c := range samples[0] {
				y := 0.0
				for j, l := range ws {
					y += ps[j][c] * l // (beep's point.Y * l)
				}
				samples[0][c] = y
			}
		}
		samples = samples[1:]
		n++
		r.pos++
	}
	return n, true
}

// at: the original sample at position x (beep's point.Y source: buf1 before off, buf2 from it)
func (r *Resampler) at(x, offBuf1 int) *[2]float64 {
	if x < r.off {
		return &r.buf1[x-offBuf1]
	}
	return &r.buf2[x-r.off]
}

// Err propagates the original Streamer's errors.
func (r *Resampler) Err() error { return r.s.Err() }

// Ratio returns the current resampling ratio.
func (r *Resampler) Ratio() float64 { return r.ratio }

// SetRatio is beep's Resampler.SetRatio.
func (r *Resampler) SetRatio(ratio float64) {
	if ratio <= 0 || math.IsInf(ratio, 0) || math.IsNaN(ratio) {
		panic(fmt.Errorf("resample: invalid ratio: %f", ratio))
	}
	r.pos *= r.ratio / ratio
	r.ratio = ratio
}
