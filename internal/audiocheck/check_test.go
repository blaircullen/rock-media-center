package audiocheck

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tristenlammi/arrmada/internal/toolrun"
)

// Fixtures are generated once per test run with ffmpeg and shared by every test in the
// package. A run without ffmpeg skips the tests that need audio.
var (
	fixtureDir  string
	fixtureMu   sync.Mutex
	fixtureMade = map[string]string{}
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "audiocheck-fixtures-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot create fixture dir:", err)
		os.Exit(1)
	}
	fixtureDir = dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := lookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := lookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
}

// ffmpegRun runs ffmpeg through toolrun and fails the test with its stderr on error.
func ffmpegRun(t *testing.T, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := toolrun.Command(ctx, toolrun.Normal, "ffmpeg", append([]string{"-v", "error", "-y"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
	}
}

// fixture returns the path of a named fixture, building it once with build.
func fixture(t *testing.T, name string, build func(t *testing.T, out string)) string {
	t.Helper()
	requireFFmpeg(t)
	fixtureMu.Lock()
	defer fixtureMu.Unlock()
	if p, ok := fixtureMade[name]; ok {
		return p
	}
	p := filepath.Join(fixtureDir, name)
	build(t, p)
	fixtureMade[name] = p
	return p
}

// noise is an lavfi white-noise source: seconds long, sample rate sr, peak amplitude amp.
func noise(seconds, sr int, amp float64) string {
	return fmt.Sprintf("anoisesrc=d=%d:r=%d:c=white:a=%g:seed=7", seconds, sr, amp)
}

// brickwall is a steep low-pass: two cascaded zero-phase FIRs, so the stop band sits well
// below the checker's -80 dB content window, the way a lossy encoder's shelf does.
func brickwall(cutoff int) string {
	one := fmt.Sprintf("firequalizer=gain='if(lt(f,%d),0,-200)':accuracy=4:zero_phase=on:fixed=on", cutoff)
	return one + "," + one
}

// buildFlac encodes lavfi source expression src through filters into a FLAC at the given
// depth (16 or 24) with channel count ch.
func buildFlac(t *testing.T, out, src, filters string, depth, ch int) {
	t.Helper()
	args := []string{"-f", "lavfi", "-i", src}
	if filters != "" {
		args = append(args, "-af", filters)
	}
	args = append(args, "-ac", fmt.Sprint(ch), "-c:a", "flac")
	if depth == 24 {
		args = append(args, "-sample_fmt", "s32", "-bits_per_raw_sample", "24")
	} else {
		args = append(args, "-sample_fmt", "s16")
	}
	ffmpegRun(t, append(args, out)...)
}

func fixtureFullband(t *testing.T) string {
	return fixture(t, "fullband.flac", func(t *testing.T, out string) {
		buildFlac(t, out, noise(20, 44100, 0.5), "", 16, 2)
	})
}

func check(t *testing.T, path string) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	return Check(ctx, path)
}

func TestFullBandNoiseIsGenuine(t *testing.T) {
	r := check(t, fixtureFullband(t))
	if r.Verdict != Genuine {
		t.Fatalf("verdict = %s (%s), want GENUINE; sig=%+v", r.Verdict, r.Reason, r.Signature)
	}
	if !r.Integrity.OK || math.Abs(r.Integrity.DecodedSec-20) > 0.1 {
		t.Errorf("integrity = %+v, want OK at ~20s", r.Integrity)
	}
	if r.Fingerprint == "" || r.LoudnessLUFS == nil {
		t.Errorf("fingerprint %q loudness %v (err %q): both must be filled", r.Fingerprint, r.LoudnessLUFS, r.LoudnessErr)
	}
}

// AE5: a transcode's 16 kHz shelf inside a FLAC container is FAKE.
func TestLowPassed16kIsFake(t *testing.T) {
	p := fixture(t, "lowpass16k.flac", func(t *testing.T, out string) {
		buildFlac(t, out, noise(20, 44100, 0.5), brickwall(16000), 16, 2)
	})
	r := check(t, p)
	if r.Verdict != Fake {
		t.Fatalf("verdict = %s (%s), want FAKE; sig=%+v", r.Verdict, r.Reason, r.Signature)
	}
	if !r.Verdict.Bad() {
		t.Error("FAKE must count as bad audio")
	}
}

// A quiet genuine FLAC is small. Under the v3 rule that alone made it SUSPECT; now the
// spectrum has to stop short too (the corroboration itself is pinned by
// TestLowBitrateOnlyCorroborates).
func TestQuietFullBandFlacStaysGenuine(t *testing.T) {
	quiet := fixture(t, "quiet-full.flac", func(t *testing.T, out string) {
		buildFlac(t, out, noise(20, 44100, 0.0005), "", 16, 1)
	})
	r := check(t, quiet)
	if r.Facts.BitrateKbps <= 0 || r.Facts.BitrateKbps >= lowBitrateKbps {
		t.Fatalf("fixture bitrate %.0f kbps is not under %.0f; the test proves nothing", r.Facts.BitrateKbps, lowBitrateKbps)
	}
	if r.Verdict != Genuine {
		t.Errorf("quiet full-band FLAC = %s (%s), want GENUINE", r.Verdict, r.Reason)
	}
}

// Content that ends at 20 kHz in a 44.1 kHz file is a 320k-class shelf.
func TestContentStoppingAt20kIsSuspect(t *testing.T) {
	shelf := fixture(t, "lowpass20k.flac", func(t *testing.T, out string) {
		buildFlac(t, out, noise(20, 44100, 0.3), brickwall(20000), 24, 2)
	})
	r := check(t, shelf)
	if r.Verdict != Suspect {
		t.Errorf("content stopping at 20 kHz = %s (%s), want SUSPECT; sig=%+v", r.Verdict, r.Reason, r.Signature)
	}
}

// The bitrate rule alone, with the spectrum numbers chosen so no shelf rule fires.
func TestLowBitrateOnlyCorroborates(t *testing.T) {
	f := Facts{Codec: "flac", SampleRate: 44100, BitrateKbps: 250}
	if v, _, _ := classify(f, Signature{TopHz: 22000, FloorDB: -40}, nil); v != Genuine {
		t.Errorf("low bitrate + full spectrum = %s, want GENUINE", v)
	}
	// 20.9 kHz top with a shallow floor: no shelf rule applies, only the corroboration.
	if v, _, _ := classify(f, Signature{TopHz: 20900, FloorDB: -40}, nil); v != Suspect {
		t.Errorf("low bitrate + top below 21 kHz = %s, want SUSPECT", v)
	}
	f.BitrateKbps = 900
	if v, _, _ := classify(f, Signature{TopHz: 20900, FloorDB: -40}, nil); v != Genuine {
		t.Errorf("normal bitrate + top 20.9 kHz and shallow floor = %s, want GENUINE", v)
	}
}

func TestClassifyThresholds(t *testing.T) {
	cd := Facts{Codec: "flac", SampleRate: 44100, BitrateKbps: 900}
	hi := Facts{Codec: "flac", SampleRate: 96000, BitrateKbps: 2500}
	cases := []struct {
		name string
		f    Facts
		sig  Signature
		want Verdict
	}{
		{"cd 16.4k", cd, Signature{TopHz: 16400, FloorDB: -90}, Fake},
		{"cd 16.5k boundary is not fake", cd, Signature{TopHz: 16500, FloorDB: -90}, Suspect},
		{"cd 19.1k", cd, Signature{TopHz: 19100, FloorDB: -10}, Suspect},
		{"cd 19.2k shallow floor", cd, Signature{TopHz: 19200, FloorDB: -60}, Genuine},
		{"cd 20k deep floor", cd, Signature{TopHz: 20000, FloorDB: -76}, Suspect},
		{"cd 20k floor -75 is not deep", cd, Signature{TopHz: 20000, FloorDB: -75}, Genuine},
		{"cd 21k top", cd, Signature{TopHz: 21000, FloorDB: -90}, Genuine},
		{"hires upsampled", hi, Signature{TopHz: 22000, FloorDB: -90}, Upsampled},
		{"hires 22.5k not upsampled", hi, Signature{TopHz: 22500, FloorDB: -10}, Genuine},
		{"hires shelf", hi, Signature{TopHz: 20500, FloorDB: -80}, Suspect},
		{"hires full", hi, Signature{TopHz: 47000, FloorDB: -50}, Genuine},
	}
	for _, c := range cases {
		if got, _, _ := classify(c.f, c.sig, nil); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	zero := &BitDepth{Bits: 24}
	if got, _, _ := classify(hi, Signature{TopHz: 47000, FloorDB: -50}, zero); got != FakeBitDepth {
		t.Errorf("zero LSBs = %s, want FAKE_BIT_DEPTH", got)
	}
}

func TestBlindIsNotBad(t *testing.T) {
	if Blind.Bad() || Genuine.Bad() {
		t.Error("BLIND and GENUINE must not count as bad audio")
	}
	for _, v := range []Verdict{Suspect, Fake, Upsampled, FakeBitDepth, Corrupt} {
		if !v.Bad() {
			t.Errorf("%s must count as bad", v)
		}
	}
}

func TestMissingFileIsBlind(t *testing.T) {
	requireFFmpeg(t)
	r := check(t, filepath.Join(t.TempDir(), "nope.flac"))
	if r.Verdict != Blind || r.Reason == "" {
		t.Fatalf("missing file = %s (%q), want BLIND with a reason", r.Verdict, r.Reason)
	}
}

// AE6: the file is fine but the storage fails mid-decode. That is BLIND, never FAKE or CORRUPT.
func TestReadFailureMidDecodeIsBlind(t *testing.T) {
	p := fixtureFullband(t)
	for _, step := range []string{"s32le", "f32le", "null"} {
		t.Run(step, func(t *testing.T) {
			c := New()
			real := c.run
			c.run = func(ctx context.Context, class toolrun.Class, out io.Writer, name string, args ...string) ([]byte, error) {
				if name == "ffmpeg" && hasArg(args, step) {
					return []byte("Error while reading: Input/output error"), fmt.Errorf("simulated read failure")
				}
				return real(ctx, class, out, name, args...)
			}
			if step == "null" {
				// loudness is non-fatal: the verdict must survive it
				r := c.Check(context.Background(), p)
				if r.Verdict != Genuine || r.LoudnessErr == "" {
					t.Fatalf("loudness failure: verdict %s, err %q; want GENUINE with LoudnessErr set", r.Verdict, r.LoudnessErr)
				}
				return
			}
			r := c.Check(context.Background(), p)
			if r.Verdict != Blind {
				t.Fatalf("verdict = %s (%s), want BLIND", r.Verdict, r.Reason)
			}
			if r.Verdict.Bad() {
				t.Error("BLIND counted as bad")
			}
		})
	}
}

func TestIOErrorTextFromDecoderIsBlindNotCorrupt(t *testing.T) {
	p := fixtureFullband(t)
	c := New()
	real := c.run
	c.run = func(ctx context.Context, class toolrun.Class, out io.Writer, name string, args ...string) ([]byte, error) {
		if name == "ffmpeg" && hasArg(args, "s32le") {
			if _, err := out.Write(make([]byte, 1024)); err != nil {
				return nil, err
			}
			return []byte("Stale file handle"), nil // exit 0 but the decoder logged a storage error
		}
		return real(ctx, class, out, name, args...)
	}
	if r := c.Check(context.Background(), p); r.Verdict != Blind {
		t.Fatalf("verdict = %s (%s), want BLIND", r.Verdict, r.Reason)
	}
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// Two levels of the same audio report loudness 6 LU apart.
func TestLoudnessTracksLevel(t *testing.T) {
	loud := fixture(t, "loud.flac", func(t *testing.T, out string) {
		buildFlac(t, out, noise(10, 44100, 0.4), "", 16, 2)
	})
	soft := fixture(t, "soft.flac", func(t *testing.T, out string) {
		buildFlac(t, out, noise(10, 44100, 0.2), "", 16, 2)
	})
	ctx := context.Background()
	a, err := Loudness(ctx, loud)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Loudness(ctx, soft)
	if err != nil {
		t.Fatal(err)
	}
	if d := a - b; math.Abs(d-6.02) > 0.2 {
		t.Errorf("loudness %.2f vs %.2f LUFS differ by %.2f LU, want ~6.0", a, b, d)
	}
}

// A 96 kHz file whose content stops at 22 kHz is a CD upsampled into a hi-res container.
func TestUpsampled96kIsUpsampled(t *testing.T) {
	p := fixture(t, "upsampled96.flac", func(t *testing.T, out string) {
		buildFlac(t, out, noise(20, 96000, 0.5), brickwall(22000), 24, 2)
	})
	r := check(t, p)
	if r.Verdict != Upsampled {
		t.Fatalf("verdict = %s (%s), want UPSAMPLED; sig=%+v", r.Verdict, r.Reason, r.Signature)
	}
}

func lookPath(name string) (string, error) { return exec.LookPath(name) }
