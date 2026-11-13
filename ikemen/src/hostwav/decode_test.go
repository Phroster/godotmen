package hostwav

import (
	"bytes"
	"encoding/binary"
	"math"
	"math/rand"
	"testing"

	"github.com/gopxl/beep/v2/wav"
)

// makeWav: a random WAVE file - PCM or WAVEFORMATEXTENSIBLE PCM, 8/16/24 bits, 1-3 channels, an extended format
// chunk or an unknown (odd-sized) chunk at times, data sizes that are not whole frames, data cut short of its header
func makeWav(rng *rand.Rand) []byte {
	var b bytes.Buffer
	le := func(v any) { binary.Write(&b, binary.LittleEndian, v) }
	bits := []int16{8, 16, 24}[rng.Intn(3)]
	chans := int16(1 + rng.Intn(3))
	bpf := int16(int(bits) / 8 * int(chans))
	rate := []int32{11025, 22050, 44100, 48000}[rng.Intn(4)]
	frames := rng.Intn(4000)
	data := make([]byte, frames*int(bpf)+rng.Intn(int(bpf)))
	rng.Read(data)
	b.WriteString("RIFF")
	le(int32(0))
	b.WriteString("WAVE")
	if rng.Intn(4) == 0 { // an unknown chunk first
		b.WriteString("LIST")
		n := int32(rng.Intn(9))
		le(n)
		pad := n
		if pad%2 != 0 {
			pad++
		}
		b.Write(make([]byte, pad))
	}
	b.WriteString("fmt ")
	if rng.Intn(5) == 0 { // WAVEFORMATEXTENSIBLE, PCM sub format
		le(int32(40))
		le(int16(-2))
		le(chans)
		le(rate)
		le(rate * int32(bpf))
		le(bpf)
		le(bits)
		le(int16(22))
		le(bits)
		le(int32(3))
		le(int32(1))
		le(int16(0))
		le(int16(0x10))
		b.Write([]byte{0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71})
	} else {
		ext := rng.Intn(3) == 0
		if ext {
			le(int32(18))
		} else {
			le(int32(16))
		}
		le(int16(1))
		le(chans)
		le(rate)
		le(rate * int32(bpf))
		le(bpf)
		le(bits)
		if ext {
			le(int16(0))
		}
	}
	b.WriteString("data")
	size := int32(len(data))
	if rng.Intn(6) == 0 {
		size += int32(rng.Intn(300)) // (the header claims more than the file has)
	}
	le(size)
	b.Write(data)
	return b.Bytes()
}

// Format, errors, Len, and every streamed sample (bit for bit), count, ok, position and error of beep's decoder and
// this one, over random files, block sizes (the resampler asks for small and odd ones) and seeks
func TestDecoderBitIdentical(t *testing.T) {
	rng := rand.New(rand.NewSource(20261017))
	samples, calls := 0, 0
	for it := 0; it < 3000; it++ {
		file := makeWav(rng)
		a, af, aerr := wav.Decode(bytes.NewReader(file))
		b, bf, berr := Decode(bytes.NewReader(file))
		if (aerr == nil) != (berr == nil) || af != bf {
			t.Fatalf("it %d: decode %v %v vs %v %v", it, af, aerr, bf, berr)
		}
		if aerr != nil {
			if aerr.Error() != berr.Error() {
				t.Fatalf("it %d: error %q vs %q", it, aerr, berr)
			}
			continue
		}
		if a.Len() != b.Len() {
			t.Fatalf("it %d: Len %d vs %d", it, a.Len(), b.Len())
		}
		bufA := make([][2]float64, 1100)
		bufB := make([][2]float64, 1100)
		for c := 0; c < 80; c++ {
			if rng.Intn(10) == 0 {
				p := rng.Intn(a.Len() + 1)
				ea, eb := a.Seek(p), b.Seek(p)
				if (ea == nil) != (eb == nil) {
					t.Fatalf("it %d: seek %v vs %v", it, ea, eb)
				}
			}
			n := rng.Intn(1025)
			if rng.Intn(3) == 0 {
				n = 1 + rng.Intn(16)
			}
			for i := range bufA[:n] {
				v := [2]float64{rng.Float64(), -rng.Float64()}
				bufA[i], bufB[i] = v, v
			}
			an, aok := a.Stream(bufA[:n])
			bn, bok := b.Stream(bufB[:n])
			calls++
			if an != bn || aok != bok || a.Position() != b.Position() || (a.Err() == nil) != (b.Err() == nil) {
				t.Fatalf("it %d call %d: %d %v %d vs %d %v %d", it, c, an, aok, a.Position(), bn, bok, b.Position())
			}
			for i := 0; i < n; i++ {
				for ch := 0; ch < 2; ch++ {
					if math.Float64bits(bufA[i][ch]) != math.Float64bits(bufB[i][ch]) {
						t.Fatalf("it %d call %d sample %d/%d: %v vs %v", it, c, i, ch, bufA[i][ch], bufB[i][ch])
					}
				}
			}
			samples += an
		}
	}
	t.Logf("%d calls, %d frames bit-identical", calls, samples)
}

// Stream does not allocate per call once its buffer has grown
func TestDecoderStreamAllocs(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var file []byte
	for {
		file = makeWav(rng)
		if d, _, err := Decode(bytes.NewReader(file)); err == nil && d.Len() > 2000 {
			break
		}
	}
	d, _, _ := Decode(bytes.NewReader(file))
	buf := make([][2]float64, 16)
	d.Stream(buf)
	if n := testing.AllocsPerRun(50, func() { d.Stream(buf) }); n != 0 {
		t.Fatalf("%v allocations per Stream", n)
	}
}
