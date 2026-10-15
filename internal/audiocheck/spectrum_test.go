package audiocheck

import (
	"math"
	"math/cmplx"
	"math/rand"
	"testing"
)

// The real FFT must equal a plain DFT.
func TestRealFFTMatchesDFT(t *testing.T) {
	const n = 64
	rng := rand.New(rand.NewSource(1))
	x := make([]float64, n)
	for i := range x {
		x[i] = rng.Float64()*2 - 1
	}
	got := make([]float64, n/2+1)
	newRealFFT(n).magnitudes(x, got)
	for k := 0; k <= n/2; k++ {
		var sum complex128
		for i, v := range x {
			sum += complex(v, 0) * cmplx.Exp(complex(0, -2*math.Pi*float64(k*i)/n))
		}
		if want := cmplx.Abs(sum); math.Abs(got[k]-want) > 1e-9 {
			t.Fatalf("bin %d = %.9f, want %.9f", k, got[k], want)
		}
	}
}

func sines(n, sr int, freqs ...float64) []float32 {
	pcm := make([]float32, n)
	for i := range pcm {
		var v float64
		for _, f := range freqs {
			v += math.Sin(2 * math.Pi * f * float64(i) / float64(sr))
		}
		pcm[i] = float32(0.5 * v / float64(len(freqs)))
	}
	return pcm
}

func TestAnalyzeSpectrumFindsContentTop(t *testing.T) {
	const sr = 44100
	// Tones up to 15 kHz and nothing above: the -80 dB top is the highest tone.
	sig, err := analyzeSpectrum(sines(sr*5, sr, 1000, 5000, 10000, 15000), sr)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(sig.TopHz-15000) > 200 {
		t.Errorf("top = %.0f Hz, want ~15000", sig.TopHz)
	}
	if sig.FloorDB > -75 {
		t.Errorf("floor = %.1f dB, want a deep floor under -75", sig.FloorDB)
	}
}

func TestAnalyzeSpectrumWhiteNoiseReachesNyquist(t *testing.T) {
	const sr = 44100
	rng := rand.New(rand.NewSource(2))
	pcm := make([]float32, sr*5)
	for i := range pcm {
		pcm[i] = float32(rng.Float64()*2 - 1)
	}
	sig, err := analyzeSpectrum(pcm, sr)
	if err != nil {
		t.Fatal(err)
	}
	if sig.TopHz < 21900 || sig.FloorDB < -20 {
		t.Errorf("white noise signature %+v, want top at Nyquist and a shallow floor", sig)
	}
}

func TestAnalyzeSpectrumErrorsAreBlindInputs(t *testing.T) {
	if _, err := analyzeSpectrum(make([]float32, 100), 44100); err == nil {
		t.Error("a decode shorter than one frame must be an error")
	}
	if _, err := analyzeSpectrum(make([]float32, 44100), 44100); err == nil {
		t.Error("digital silence must be an error, not GENUINE")
	}
}
