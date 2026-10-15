// Package audiocheck decides whether an audio file is genuinely what it claims to be, so a
// fake lossless file (a lossy transcode in a FLAC container, a 16-bit file padded to 24-bit,
// a 44.1 kHz file upsampled to 96 kHz) never reaches the library. It is a Go port of the
// getmusic skill's flac-check.py v3 with two changes (plan KTD5): the bit-depth check reads
// windows across the whole file, and a low FLAC bitrate only corroborates a spectral finding.
//
// Beyond the verdict it returns what the swap logic needs (plan KTD19): the file's real
// facts (R16), a full-decode integrity result (R12), a sha256 fingerprint of the decoded
// audio (R25), the spectral signature, and EBU R128 integrated loudness.
//
// Every ffmpeg and ffprobe call goes through internal/toolrun with a deadline. A call that
// could not look at the file (probe failure, I/O error, timeout, empty decode) yields BLIND,
// never FAKE: BLIND is "unknown", retried by the caller, and does not count as bad audio.
package audiocheck

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tristenlammi/arrmada/internal/toolrun"
)

// Verdict is the checker's classification of one file.
type Verdict string

const (
	// Genuine: the spectrum, bit depth and decode all look like the file claims.
	Genuine Verdict = "GENUINE"
	// Suspect: a lossy-transcode shelf below the expected band, or a low bitrate together
	// with a spectrum that stops short of 21 kHz.
	Suspect Verdict = "SUSPECT"
	// Fake: content stops below 16.5 kHz (128k-class transcode).
	Fake Verdict = "FAKE"
	// Upsampled: a 44.1 kHz source in a 48 kHz-or-higher container.
	Upsampled Verdict = "UPSAMPLED"
	// FakeBitDepth: a >16-bit container whose least-significant bits are all zero.
	FakeBitDepth Verdict = "FAKE_BIT_DEPTH"
	// Corrupt: the file decodes with errors or to the wrong length.
	Corrupt Verdict = "CORRUPT"
	// Blind: the check could not read the file. Unknown, not bad.
	Blind Verdict = "BLIND"
)

// Bad reports whether the verdict is evidence against the audio. Blind is not evidence.
func (v Verdict) Bad() bool {
	switch v {
	case Suspect, Fake, Upsampled, FakeBitDepth, Corrupt:
		return true
	}
	return false
}

// Thresholds. They equal flac-check.py v3's unless a comment says otherwise.
const (
	// FFT framing for the spectrum: 8192-point Hann frames, hop 4096, over SpectrumSeconds
	// taken from the middle of the file.
	fftSize         = 8192
	fftHop          = 4096
	spectrumSeconds = 60.0

	// contentRangeDB is how far below the peak a bin may sit and still count as content
	// (v3's "-80 dB spectral shelf window").
	contentRangeDB = 80.0

	// FakeTopHz: at or below 44.1 kHz, content ending under this is a 128k-class transcode.
	fakeTopHz = 16500.0
	// suspect192TopHz: content ending under this is a 192k-class (or lower) shelf.
	suspect192TopHz = 19200.0
	// shelfTopHz and shelfFloorDB: content ending under shelfTopHz with a near-Nyquist
	// floor under shelfFloorDB (relative to peak) is a 320k-class shelf. shelfTopHz is also
	// the "spectrum stops short" test that lets a low FLAC bitrate corroborate (KTD5).
	shelfTopHz   = 21000.0
	shelfFloorDB = -75.0
	// upsampledNyquistHz and upsampledToleranceHz: in a container above 44.1 kHz, content
	// ending within the tolerance of the CD Nyquist is an upsampled CD.
	upsampledNyquistHz   = 22050.0
	upsampledToleranceHz = 400.0
	// floorBandLowHz and floorBandHighHz bound the near-Nyquist band (below Nyquist) whose
	// mean level gives the floor.
	floorBandLowHz  = 1000.0
	floorBandHighHz = 100.0

	// lowBitrateKbps: a FLAC under this is unusually small. In v3 this alone made a file
	// SUSPECT; here it only counts when the spectrum also stops short (KTD5).
	lowBitrateKbps = 400.0

	// silenceFloorDB: a window whose strongest averaged bin is quieter than this carries no
	// signal, so there is nothing to classify (BLIND, not GENUINE).
	silenceFloorDB = -120.0
)

// Bit-depth sampling (KTD5): lsbWindows windows of lsbWindowSeconds spread evenly over the
// file, or one read of the whole file when it is no longer than all the windows together.
const (
	lsbWindows       = 12
	lsbWindowSeconds = 5.0
)

// Integrity: a decoded length further than this from the probed duration fails. Lossless
// codecs have exact lengths; lossy duration estimates and decoder priming are looser.
const (
	losslessLengthToleranceSec = 0.25
	lossyLengthToleranceSec    = 1.5
)

// Per-call deadlines. v3 used 180 s for everything; a full decode of a long hi-res file
// needs more.
const (
	probeTimeout     = 30 * time.Second
	analysisTimeout  = 180 * time.Second
	fullPassTimeout  = 10 * time.Minute
	maxStderrCapture = 64 << 10
)

// Facts are the file's real properties from ffprobe (R16).
type Facts struct {
	Codec       string  `json:"codec"`
	Container   string  `json:"container"`
	SampleFmt   string  `json:"sample_fmt,omitempty"`
	BitDepth    int     `json:"bit_depth"` // 0 when the codec has none (lossy)
	SampleRate  int     `json:"sample_rate"`
	Channels    int     `json:"channels"`
	DurationSec float64 `json:"duration_sec"`
	SizeBytes   int64   `json:"size_bytes"`
	BitrateKbps float64 `json:"bitrate_kbps"`
}

// Signature is the spectral signature compared across files (KTD19).
type Signature struct {
	TopHz   float64 `json:"top_hz"`   // highest frequency within 80 dB of the peak
	FloorDB float64 `json:"floor_db"` // near-Nyquist level relative to the peak (dB, negative)
	PeakDB  float64 `json:"peak_db"`
}

// BitDepth is the least-significant-bit finding for a >16-bit file.
type BitDepth struct {
	Bits          int  `json:"bits"`
	LSBActive     bool `json:"lsb_active"`
	WindowsRead   int  `json:"windows_read"`
	WindowsActive int  `json:"windows_active"`
}

// Integrity is the full-decode result (R12).
type Integrity struct {
	OK         bool    `json:"ok"`
	DecodedSec float64 `json:"decoded_sec"`
	Detail     string  `json:"detail,omitempty"`
}

// Result is everything one Check learned. Sections after a failed step are zero values.
type Result struct {
	Path    string  `json:"path"`
	Verdict Verdict `json:"verdict"`
	// Reason is the one-line why behind the verdict; for Blind it names what could not be read.
	Reason string   `json:"reason"`
	Notes  []string `json:"notes,omitempty"`

	Facts     Facts      `json:"facts"`
	Integrity Integrity  `json:"integrity"`
	Signature *Signature `json:"signature,omitempty"`
	BitDepth  *BitDepth  `json:"bit_depth,omitempty"`
	// Fingerprint is the sha256 (hex) of the decoded PCM at the file's native rate and
	// channel layout; identical audio in another container or release hashes the same.
	Fingerprint string `json:"fingerprint,omitempty"`
	// LoudnessLUFS is EBU R128 integrated loudness; LoudnessErr says why it is absent.
	LoudnessLUFS *float64 `json:"loudness_lufs,omitempty"`
	LoudnessErr  string   `json:"loudness_err,omitempty"`
}

// Line is a one-line report in the shape of flac-check.py's output.
func (r Result) Line() string {
	f := r.Facts
	return fmt.Sprintf("%s: %s | %d Hz %d-bit %dch %.0f kbps %.0fs | %s | %s",
		r.Path, r.Verdict, f.SampleRate, f.BitDepth, f.Channels, f.BitrateKbps, f.DurationSec,
		r.Reason, strings.Join(r.Notes, "; "))
}

// runner starts one helper tool, streaming its stdout into stdout, and returns its stderr
// (capped). It exists so tests can simulate an I/O failure mid-decode.
type runner func(ctx context.Context, class toolrun.Class, stdout io.Writer, name string, args ...string) (stderr []byte, err error)

// Checker runs the checks. The zero value is not usable; use New.
type Checker struct {
	// Class is the toolrun priority for the long full-file passes. Probes always run Normal.
	Class toolrun.Class
	run   runner
}

// New returns a Checker that runs its decodes at background priority so Plex wins the box.
func New() *Checker {
	return &Checker{Class: toolrun.Background, run: runTool}
}

// Check runs every check against path and never returns an error: a failure to look is
// the Blind verdict with the reason recorded.
func Check(ctx context.Context, path string) Result { return New().Check(ctx, path) }

func runTool(ctx context.Context, class toolrun.Class, stdout io.Writer, name string, args ...string) ([]byte, error) {
	cmd := toolrun.Command(ctx, class, name, args...)
	cmd.Stdout = stdout
	errBuf := &cappedBuffer{limit: maxStderrCapture}
	cmd.Stderr = errBuf
	err := cmd.Run()
	return errBuf.Bytes(), err
}

// call runs a tool under its own deadline. A failure keeps the tool's stderr in the error
// so the cause is never reduced to "exit status 1".
func (c *Checker) call(ctx context.Context, timeout time.Duration, class toolrun.Class, stdout io.Writer, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stderr, err := c.run(ctx, class, stdout, name, args...)
	if err != nil {
		if ctx.Err() != nil {
			return stderr, fmt.Errorf("%s: %w (deadline %s)", name, ctx.Err(), timeout)
		}
		return stderr, fmt.Errorf("%s: %w: %s", name, err, tail(stderr))
	}
	return stderr, nil
}

// Check is the full checker. See the package comment.
func (c *Checker) Check(ctx context.Context, path string) Result {
	res := Result{Path: path}
	blind := func(reason string) Result {
		res.Verdict, res.Reason = Blind, reason
		return res
	}

	facts, err := c.probe(ctx, path)
	if err != nil {
		return blind("probe failed: " + err.Error())
	}
	res.Facts = facts

	integ, fp, err := c.decodeAll(ctx, path, facts)
	if err != nil {
		return blind("full decode could not read the file: " + err.Error())
	}
	res.Integrity, res.Fingerprint = integ, fp
	if !integ.OK {
		res.Verdict, res.Reason = Corrupt, integ.Detail
		return res
	}

	sig, err := c.spectrum(ctx, path, facts)
	if err != nil {
		return blind("spectrum: " + err.Error())
	}
	res.Signature = &sig

	if checksLSB(facts) {
		bd, err := c.bitDepth(ctx, path, facts)
		if err != nil {
			return blind("bit depth: " + err.Error())
		}
		res.BitDepth = &bd
	}

	// Loudness only feeds the swap comparison; failing to measure it must not hide the verdict.
	if l, err := c.loudness(ctx, path); err != nil {
		res.LoudnessErr = err.Error()
	} else {
		res.LoudnessLUFS = &l
	}

	res.Verdict, res.Reason, res.Notes = classify(facts, sig, res.BitDepth)
	return res
}

// classify turns measurements into a verdict. It is pure so every threshold is testable
// without audio.
func classify(f Facts, sig Signature, bd *BitDepth) (Verdict, string, []string) {
	nyq := float64(f.SampleRate) / 2
	notes := []string{fmt.Sprintf("content to %.1f kHz (nyq %.1f), top-band floor %.0f dB rel peak",
		sig.TopHz/1000, nyq/1000, sig.FloorDB)}
	verdict, reason := Genuine, "spectrum reaches the expected band"

	if f.SampleRate <= 44100 {
		switch {
		case sig.TopHz < fakeTopHz:
			verdict, reason = Fake, "128k-class shelf"
		case sig.TopHz < suspect192TopHz:
			verdict, reason = Suspect, "<=192k-class shelf"
		case sig.TopHz < shelfTopHz && sig.FloorDB < shelfFloorDB:
			verdict, reason = Suspect, "320k-class shelf"
		}
	} else {
		switch {
		case absf(sig.TopHz-upsampledNyquistHz) < upsampledToleranceHz:
			verdict, reason = Upsampled, "44.1k source in hi-res container"
		case sig.TopHz < shelfTopHz && sig.FloorDB < shelfFloorDB:
			verdict, reason = Suspect, fmt.Sprintf("lossy shelf %.1f kHz", sig.TopHz/1000)
		}
	}

	if bd != nil {
		state := "active"
		if !bd.LSBActive {
			state = "ALL ZERO"
		}
		notes = append(notes, fmt.Sprintf("%d-bit, true LSBs %s (%d/%d windows)", bd.Bits, state, bd.WindowsActive, bd.WindowsRead))
		if !bd.LSBActive {
			verdict, reason = FakeBitDepth, fmt.Sprintf("padded %d-bit container, no LSB content", bd.Bits)
		}
	}

	// Low bitrate corroborates only: a quiet genuine FLAC with a full spectrum stays Genuine.
	if f.Codec == "flac" && f.BitrateKbps > 0 && f.BitrateKbps < lowBitrateKbps {
		notes = append(notes, fmt.Sprintf("LOW bitrate %.0f kbps", f.BitrateKbps))
		if verdict == Genuine && sig.TopHz < shelfTopHz {
			verdict, reason = Suspect, "low bitrate and spectrum stops below 21 kHz"
		}
	}
	return verdict, reason, notes
}

func absf(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// cappedBuffer keeps at most limit bytes of a tool's output; a corrupt file can make a
// decoder print one error per frame.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *cappedBuffer) Bytes() []byte { return b.buf.Bytes() }

// tail is the last part of a tool's stderr, for error messages.
func tail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 400 {
		s = "..." + s[len(s)-400:]
	}
	return s
}
