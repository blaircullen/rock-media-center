package audiocheck

import "testing"

// zero LSBs: a 16-bit source stored in a 24-bit container.
func fixtureFake24(t *testing.T) string {
	return fixture(t, "fake24.flac", func(t *testing.T, out string) {
		wav16 := out + ".s16.wav"
		ffmpegRun(t, "-f", "lavfi", "-i", noise(20, 44100, 0.5), "-ac", "2", "-c:a", "pcm_s16le", wav16)
		ffmpegRun(t, "-i", wav16, "-c:a", "flac", "-sample_fmt", "s32", "-bits_per_raw_sample", "24", out)
	})
}

func TestZeroLSBs24BitIsFakeBitDepth(t *testing.T) {
	r := check(t, fixtureFake24(t))
	if r.Facts.BitDepth != 24 {
		t.Fatalf("fixture is %d-bit, want 24", r.Facts.BitDepth)
	}
	if r.Verdict != FakeBitDepth {
		t.Fatalf("verdict = %s (%s), want FAKE_BIT_DEPTH; bit depth %+v", r.Verdict, r.Reason, r.BitDepth)
	}
	if r.BitDepth == nil || r.BitDepth.LSBActive || r.BitDepth.WindowsRead == 0 {
		t.Errorf("bit depth = %+v, want every read window all-zero", r.BitDepth)
	}
}

// 40 s of digital silence before the music, in a file long enough that the check samples
// windows instead of reading it all. v3 read only the first 30 s and called this FAKE.
func TestGenuine24BitWithSilentIntroIsGenuine(t *testing.T) {
	p := fixture(t, "silent-intro24.flac", func(t *testing.T, out string) {
		buildFlac(t, out, noise(150, 44100, 0.3), "volume=enable='lt(t,40)':volume=0", 24, 2)
	})
	r := check(t, p)
	if r.Facts.DurationSec <= lsbWindows*lsbWindowSeconds {
		t.Fatalf("fixture is %.0fs; it must exceed the windowed threshold", r.Facts.DurationSec)
	}
	if r.Verdict != Genuine {
		t.Fatalf("verdict = %s (%s), want GENUINE; bit depth %+v", r.Verdict, r.Reason, r.BitDepth)
	}
	if r.BitDepth == nil || !r.BitDepth.LSBActive {
		t.Fatalf("bit depth = %+v, want active LSBs", r.BitDepth)
	}
	// And it did read the early silent windows first: v3's 30 s read would have seen only zeros.
	if r.BitDepth.WindowsRead < 2 {
		t.Errorf("only %d window(s) read; the silent-intro windows should have been read before one was active", r.BitDepth.WindowsRead)
	}
}

func TestLSBWindowsSpanTheWholeFile(t *testing.T) {
	if ws := lsbWindowStarts(30); len(ws) != 1 || ws[0].length != 0 {
		t.Errorf("a short file is one whole-file read, got %+v", ws)
	}
	ws := lsbWindowStarts(300)
	if len(ws) != lsbWindows {
		t.Fatalf("%d windows, want %d", len(ws), lsbWindows)
	}
	if ws[0].start != 0 {
		t.Errorf("first window starts at %.1f, want 0", ws[0].start)
	}
	last := ws[len(ws)-1]
	if last.start+last.length != 300 {
		t.Errorf("last window ends at %.1f, want the end of the file (300)", last.start+last.length)
	}
}

func TestChecksLSBOnlyForWideIntegerPCM(t *testing.T) {
	cases := []struct {
		f    Facts
		want bool
	}{
		{Facts{BitDepth: 16, SampleFmt: "s16"}, false},
		{Facts{BitDepth: 0}, false},
		{Facts{BitDepth: 24, SampleFmt: "s32"}, true},
		{Facts{BitDepth: 32, SampleFmt: "s32"}, true},
		{Facts{BitDepth: 32, SampleFmt: "flt"}, false},
	}
	for _, c := range cases {
		if got := checksLSB(c.f); got != c.want {
			t.Errorf("checksLSB(%+v) = %v, want %v", c.f, got, c.want)
		}
	}
}
