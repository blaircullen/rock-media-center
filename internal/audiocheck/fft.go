package audiocheck

import (
	"math"
	"math/bits"
)

// fftPlan holds the twiddle factors and bit-reversal table for one power-of-two size, so
// the 600-odd frames of a 60 s window share one setup.
type fftPlan struct {
	n       int
	cos     []float64 // cos(2πk/n), k < n/2
	sin     []float64 // -sin(2πk/n), k < n/2 (forward transform)
	reverse []int
}

// newFFTPlan builds a plan for a power-of-two size n (n >= 2).
func newFFTPlan(n int) *fftPlan {
	if n < 2 || n&(n-1) != 0 {
		panic("audiocheck: FFT size must be a power of two")
	}
	p := &fftPlan{n: n, cos: make([]float64, n/2), sin: make([]float64, n/2), reverse: make([]int, n)}
	for k := 0; k < n/2; k++ {
		ang := 2 * math.Pi * float64(k) / float64(n)
		p.cos[k] = math.Cos(ang)
		p.sin[k] = -math.Sin(ang)
	}
	shift := uint(bits.UintSize - bits.Len(uint(n-1)))
	for i := range p.reverse {
		p.reverse[i] = int(bits.Reverse(uint(i)) >> shift)
	}
	return p
}

// transform runs an in-place iterative radix-2 FFT over (re, im), both of length n.
func (p *fftPlan) transform(re, im []float64) {
	n := p.n
	for i := 0; i < n; i++ {
		if j := p.reverse[i]; j > i {
			re[i], re[j] = re[j], re[i]
			im[i], im[j] = im[j], im[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		half := size / 2
		step := n / size
		for start := 0; start < n; start += size {
			for k := 0; k < half; k++ {
				wr, wi := p.cos[k*step], p.sin[k*step]
				a, b := start+k, start+k+half
				tr := re[b]*wr - im[b]*wi
				ti := re[b]*wi + im[b]*wr
				re[b], im[b] = re[a]-tr, im[a]-ti
				re[a], im[a] = re[a]+tr, im[a]+ti
			}
		}
	}
}

// realFFT computes magnitude spectra of real input. A real input of size n is packed as n/2
// complex samples (even samples as the real part, odd as the imaginary part), transformed at
// half size, then split into the real spectrum. Not safe for concurrent use (it owns scratch).
type realFFT struct {
	n      int
	half   *fftPlan
	re, im []float64
	wr, wi []float64 // exp(-2πik/n), k <= n/2
}

// newRealFFT builds a real transform of power-of-two size n (n >= 4).
func newRealFFT(n int) *realFFT {
	h := n / 2
	r := &realFFT{n: n, half: newFFTPlan(h), re: make([]float64, h), im: make([]float64, h),
		wr: make([]float64, h+1), wi: make([]float64, h+1)}
	for k := 0; k <= h; k++ {
		ang := -2 * math.Pi * float64(k) / float64(n)
		r.wr[k], r.wi[k] = math.Cos(ang), math.Sin(ang)
	}
	return r
}

// magnitudes writes |FFT(x)| for bins 0..n/2 into out (len n/2+1) — the same values as
// numpy's abs(rfft(x)). x must have length n.
func (r *realFFT) magnitudes(x, out []float64) {
	h := r.n / 2
	for i := 0; i < h; i++ {
		r.re[i] = x[2*i]
		r.im[i] = x[2*i+1]
	}
	r.half.transform(r.re, r.im)
	for k := 0; k <= h; k++ {
		k1, k2 := k%h, (h-k)%h
		// E = (Z[k] + conj(Z[h-k]))/2 is the even-sample spectrum, O = (Z[k] - conj(Z[h-k]))/(2i) the odd.
		er := (r.re[k1] + r.re[k2]) / 2
		ei := (r.im[k1] - r.im[k2]) / 2
		or := (r.im[k1] + r.im[k2]) / 2
		oi := -(r.re[k1] - r.re[k2]) / 2
		xr := er + or*r.wr[k] - oi*r.wi[k]
		xi := ei + or*r.wi[k] + oi*r.wr[k]
		out[k] = math.Hypot(xr, xi)
	}
}
