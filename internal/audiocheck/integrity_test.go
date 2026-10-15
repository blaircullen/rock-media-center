package audiocheck

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestTruncatedFlacFailsIntegrity(t *testing.T) {
	full := fixtureFullband(t)
	b, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	cut := filepath.Join(t.TempDir(), "cut.flac")
	if err := os.WriteFile(cut, b[:len(b)*6/10], 0o644); err != nil {
		t.Fatal(err)
	}
	r := check(t, cut)
	if r.Verdict != Corrupt {
		t.Fatalf("verdict = %s (%s), want CORRUPT", r.Verdict, r.Reason)
	}
	if r.Integrity.OK || r.Integrity.Detail == "" {
		t.Errorf("integrity = %+v, want a failure with a reason", r.Integrity)
	}
	if !r.Verdict.Bad() {
		t.Error("CORRUPT must count as bad")
	}
}

// Same PCM in FLAC and WAV, and a 24-bit version that is the same audio, share a fingerprint
// when the samples are identical; different audio does not.
func TestFingerprintIgnoresContainer(t *testing.T) {
	wav := fixture(t, "fp.wav", func(t *testing.T, out string) {
		ffmpegRun(t, "-f", "lavfi", "-i", noise(8, 44100, 0.5), "-ac", "2", "-c:a", "pcm_s16le", out)
	})
	flac := fixture(t, "fp.flac", func(t *testing.T, out string) {
		ffmpegRun(t, "-i", wav, "-c:a", "flac", out)
	})
	ctx := context.Background()
	a, err := Fingerprint(ctx, wav)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Fingerprint(ctx, flac)
	if err != nil {
		t.Fatal(err)
	}
	if a != b || len(a) != 64 {
		t.Errorf("wav %q vs flac %q: want the same 64-char sha256", a, b)
	}
	other, err := Fingerprint(ctx, fixtureFullband(t))
	if err != nil {
		t.Fatal(err)
	}
	if other == a {
		t.Error("different audio produced the same fingerprint")
	}
}

func TestFingerprintOfCorruptFileIsAnError(t *testing.T) {
	requireFFmpeg(t)
	bad := filepath.Join(t.TempDir(), "junk.flac")
	if err := os.WriteFile(bad, []byte("fLaC not really a flac file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if fp, err := Fingerprint(context.Background(), bad); err == nil {
		t.Errorf("fingerprint of junk = %q, want an error", fp)
	}
}

func TestLengthTolerance(t *testing.T) {
	if lengthTolerance("flac") >= lengthTolerance("mp3") {
		t.Error("lossless codecs must be held to a tighter length than lossy ones")
	}
	if lengthTolerance("pcm_s24le") != lengthTolerance("flac") {
		t.Error("PCM is lossless")
	}
}
