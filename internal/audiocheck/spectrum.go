package audiocheck

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// spectrum decodes spectrumSeconds from the middle of the file to mono float and measures
// its signature, as flac-check.py's decode_mid + spectrum do.
func (c *Checker) spectrum(ctx context.Context, path string, f Facts) (Signature, error) {
	start := f.DurationSec/2 - spectrumSeconds/2
	if start < 0 {
		start = 0
	}
	var out bytes.Buffer
	_, err := c.call(ctx, analysisTimeout, c.Class, &out, "ffmpeg",
		"-v", "error", "-ss", fmt.Sprintf("%.1f", start), "-t", fmt.Sprintf("%.0f", spectrumSeconds),
		"-i", path, "-map", "0:a:0", "-ac", "1", "-f", "f32le", "-")
	if err != nil {
		return Signature{}, err
	}
	return analyzeSpectrum(floatsFromLE(out.Bytes()), f.SampleRate)
}

// floatsFromLE reads little-endian float32 PCM.
func floatsFromLE(b []byte) []float32 {
	n := len(b) / 4
	pcm := make([]float32, n)
	for i := range pcm {
		pcm[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return pcm
}

// analyzeSpectrum averages Hann-windowed fftSize-point magnitude frames (hop fftHop) and
// reads the signature off the averaged spectrum in dB. Fewer than one frame of audio, or no
// signal at all, is an error (the caller reports BLIND): there is nothing to classify.
func analyzeSpectrum(pcm []float32, sampleRate int) (Signature, error) {
	if len(pcm) < fftSize {
		return Signature{}, fmt.Errorf("empty or short decode (%d samples) - unreadable or truncated", len(pcm))
	}
	// numpy.hanning(M): 0.5 - 0.5 cos(2πn/(M-1))
	win := make([]float64, fftSize)
	for i := range win {
		win[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(fftSize-1))
	}
	fft := newRealFFT(fftSize)
	frame := make([]float64, fftSize)
	mag := make([]float64, fftSize/2+1)
	acc := make([]float64, fftSize/2+1)
	frames := 0
	// Same bound as v3's range(0, len-win, hop): a frame that ends exactly at the end is dropped.
	for off := 0; off < len(pcm)-fftSize; off += fftHop {
		for i := range frame {
			frame[i] = float64(pcm[off+i]) * win[i]
		}
		fft.magnitudes(frame, mag)
		for k, m := range mag {
			acc[k] += m
		}
		frames++
	}
	if frames == 0 {
		return Signature{}, errors.New("no full analysis frame in the decode")
	}
	db := make([]float64, len(acc))
	peak := math.Inf(-1)
	for k, a := range acc {
		db[k] = 20 * math.Log10(a/float64(frames)+1e-12)
		if db[k] > peak {
			peak = db[k]
		}
	}
	if peak < silenceFloorDB {
		return Signature{}, errors.New("window is digital silence - no signal to classify")
	}
	binHz := float64(sampleRate) / fftSize
	// FloorDB stays 0 (no shelf evidence) when the sample rate leaves no near-Nyquist band.
	sig := Signature{PeakDB: peak}
	for k := range db {
		if db[k] > peak-contentRangeDB {
			sig.TopHz = float64(k) * binHz
		}
	}
	nyq := float64(sampleRate) / 2
	var sum float64
	var n int
	for k := range db {
		if hz := float64(k) * binHz; hz > nyq-floorBandLowHz && hz < nyq-floorBandHighHz {
			sum += db[k]
			n++
		}
	}
	if n > 0 {
		sig.FloorDB = sum/float64(n) - peak
	}
	return sig, nil
}
