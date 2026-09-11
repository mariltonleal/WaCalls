package siptrunk

import (
	"math"
	"testing"
)

func sine(n int, freq, rate float64, amp float32) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = amp * float32(math.Sin(2*math.Pi*freq*float64(i)/rate))
	}
	return out
}

func rms16(s []int16) float64 {
	var acc float64
	for _, v := range s {
		f := float64(v) / 32768
		acc += f * f
	}
	return math.Sqrt(acc / float64(len(s)))
}

func TestDownsamplerKeepsVoiceBand(t *testing.T) {
	d := NewDownsampler()
	in := sine(960*10, 1000, 16000, 0.5)
	var out []int16
	for i := 0; i < len(in); i += 960 {
		out = append(out, d.Process(in[i:i+960])...)
	}
	if len(out) != len(in)/2 {
		t.Fatalf("got %d samples, want %d", len(out), len(in)/2)
	}
	// Skip the filter warm-up, then a 1 kHz tone must come through near full level.
	got := rms16(out[480:])
	want := 0.5 / math.Sqrt2
	if math.Abs(got-want) > 0.05 {
		t.Fatalf("1 kHz rms = %.3f, want ≈ %.3f", got, want)
	}
}

func TestDownsamplerRejectsAboveNyquist(t *testing.T) {
	d := NewDownsampler()
	in := sine(960*10, 7000, 16000, 0.5)
	var out []int16
	for i := 0; i < len(in); i += 960 {
		out = append(out, d.Process(in[i:i+960])...)
	}
	if got := rms16(out[480:]); got > 0.02 {
		t.Fatalf("7 kHz leaked through: rms = %.3f", got)
	}
}

func TestDownsamplerOddFrames(t *testing.T) {
	d := NewDownsampler()
	total := 0
	for _, n := range []int{3, 4, 5, 8} {
		total += len(d.Process(make([]float32, n)))
	}
	if total != 10 {
		t.Fatalf("got %d samples for 20 inputs, want 10", total)
	}
}

func TestUpsamplerDoubles(t *testing.T) {
	u := NewUpsampler()
	out := u.Process([]int16{0, 16384, 16384})
	if len(out) != 6 {
		t.Fatalf("got %d samples, want 6", len(out))
	}
	// Second output pair interpolates between 0 and 0.5.
	if math.Abs(float64(out[2])-0.25) > 1e-3 || math.Abs(float64(out[3])-0.5) > 1e-3 {
		t.Fatalf("unexpected interpolation: %v", out)
	}
}

func TestPCMByteRoundTrip(t *testing.T) {
	in := []int16{-32768, -1, 0, 1, 32767}
	got := bytesLEToInt16(int16ToBytesLE(in))
	for i := range in {
		if got[i] != in[i] {
			t.Fatalf("sample %d: got %d want %d", i, got[i], in[i])
		}
	}
}
