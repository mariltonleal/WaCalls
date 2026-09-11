package siptrunk

import (
	"encoding/binary"
	"math"
)

// Downsampler converts 16 kHz float32 PCM into 8 kHz int16 PCM. A windowed
// sinc low-pass (cut-off ~3.6 kHz) runs before decimation so the WhatsApp
// wideband audio does not alias into the telephone band.
type Downsampler struct {
	taps []float32
	hist []float32 // last len(taps)-1 input samples
	odd  *float32  // carried input sample when a frame has an odd length
}

const downsampleTaps = 31

func NewDownsampler() *Downsampler {
	taps := make([]float32, downsampleTaps)
	// Normalised cut-off as a fraction of the 16 kHz input rate: 3.6 kHz.
	const fc = 3600.0 / 16000.0
	m := float64(downsampleTaps - 1)
	var sum float64
	for n := 0; n < downsampleTaps; n++ {
		x := float64(n) - m/2
		var h float64
		if x == 0 {
			h = 2 * fc
		} else {
			h = math.Sin(2*math.Pi*fc*x) / (math.Pi * x)
		}
		// Hamming window.
		h *= 0.54 - 0.46*math.Cos(2*math.Pi*float64(n)/m)
		taps[n] = float32(h)
		sum += h
	}
	for n := range taps {
		taps[n] = float32(float64(taps[n]) / sum)
	}
	return &Downsampler{taps: taps, hist: make([]float32, downsampleTaps-1)}
}

// Process filters and decimates in by 2. It returns len(in)/2 samples (an
// odd trailing sample is carried over to the next call).
func (d *Downsampler) Process(in []float32) []int16 {
	if d.odd != nil {
		in = append([]float32{*d.odd}, in...)
		d.odd = nil
	}
	if len(in)%2 == 1 {
		last := in[len(in)-1]
		d.odd = &last
		in = in[:len(in)-1]
	}
	if len(in) == 0 {
		return nil
	}
	buf := make([]float32, 0, len(d.hist)+len(in))
	buf = append(buf, d.hist...)
	buf = append(buf, in...)

	nTaps := len(d.taps)
	out := make([]int16, 0, len(in)/2)
	// Output sample k corresponds to input sample 2k (filter centred on it).
	for i := nTaps - 1; i < len(buf); i += 2 {
		var acc float32
		for k := 0; k < nTaps; k++ {
			acc += d.taps[k] * buf[i-k]
		}
		out = append(out, floatToInt16(acc))
	}
	copy(d.hist, buf[len(buf)-len(d.hist):])
	return out
}

// Upsampler converts 8 kHz int16 PCM into 16 kHz float32 PCM by linear
// interpolation. Good enough for narrowband speech going into MLow.
type Upsampler struct {
	prev float32
}

func NewUpsampler() *Upsampler { return &Upsampler{} }

func (u *Upsampler) Process(in []int16) []float32 {
	out := make([]float32, 0, len(in)*2)
	for _, s := range in {
		cur := float32(s) / 32768.0
		out = append(out, (u.prev+cur)*0.5, cur)
		u.prev = cur
	}
	return out
}

func floatToInt16(s float32) int16 {
	switch {
	case s != s: // NaN
		return 0
	case s >= 1:
		return math.MaxInt16
	case s <= -1:
		return math.MinInt16
	}
	return int16(s * 32767)
}

func int16ToBytesLE(in []int16) []byte {
	out := make([]byte, len(in)*2)
	for i, s := range in {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(s))
	}
	return out
}

func bytesLEToInt16(in []byte) []int16 {
	out := make([]int16, len(in)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(in[i*2:]))
	}
	return out
}
