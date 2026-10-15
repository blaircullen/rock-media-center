package musicengine

import (
	"strings"
	"testing"
)

func cmpLufs(v float64) *float64 { return &v }

// cmpBase is a genuine 16-bit/44.1 kHz FLAC, a 200 s track at -9 LUFS from release group rg1.
func cmpBase(path, fp string) CmpFacts {
	return CmpFacts{
		Path: path, Codec: "flac", BitDepth: 16, SampleRate: 44100, BitrateKbps: 900,
		Verdict: "GENUINE", Fingerprint: fp, TopHz: 21000, LengthMs: 200000,
		LoudnessLUFS: cmpLufs(-9), ReleaseGroupMBID: "rg1",
	}
}

func cmpIn(inc, cand CmpFacts) CmpInput {
	return CmpInput{Incumbent: inc, Candidate: cand, AutoSwaps: true}
}

func TestCmpDecide(t *testing.T) {
	inc := func() CmpFacts { return cmpBase("/lib/a.flac", "fp-inc") }
	cand := func() CmpFacts { return cmpBase("/stage/a.flac", "fp-cand") }

	tests := []struct {
		name   string
		mutate func(in *CmpInput)
		want   CmpAction
		kind   string
		reason string // substring of the reason
	}{
		{"24-bit candidate over 16-bit incumbent swaps", func(in *CmpInput) { in.Candidate.BitDepth = 24 }, CmpSwap, "", "bit depth"},
		{"higher sample rate swaps", func(in *CmpInput) { in.Candidate.SampleRate = 96000 }, CmpSwap, "", "sample rate"},
		{"identical quality is not an upgrade", func(in *CmpInput) {}, CmpKeep, "", "no better"},
		{"lower bit depth is not an upgrade", func(in *CmpInput) { in.Incumbent.BitDepth = 24 }, CmpKeep, "", "worse in bit depth"},
		// AE7
		{"16/96 incumbent against 24/44.1 candidate is a trade-off", func(in *CmpInput) {
			in.Incumbent.SampleRate = 96000
			in.Candidate.BitDepth = 24
		}, CmpAsk, DecisionTradeOff, "better in bit depth but worse in sample rate"},
		{"approval turns a trade-off into a swap", func(in *CmpInput) {
			in.Incumbent.SampleRate = 96000
			in.Candidate.BitDepth = 24
			in.Approved = true
		}, CmpSwap, "", "approved"},
		// AE3
		{"SUSPECT incumbent and a genuine candidate swap even at the same quality", func(in *CmpInput) {
			in.Incumbent.Verdict, in.Incumbent.TopHz = "SUSPECT", 19000
		}, CmpSwap, "", "SUSPECT"},
		{"FAKE incumbent and a genuine candidate swap even when its nominal quality is lower", func(in *CmpInput) {
			in.Incumbent.Verdict, in.Incumbent.TopHz, in.Incumbent.BitDepth = "FAKE", 16000, 24
		}, CmpSwap, "", "FAKE"},
		{"a lossy candidate does not use the FAKE override", func(in *CmpInput) {
			in.Incumbent.Verdict, in.Incumbent.TopHz = "FAKE", 16000
			in.Candidate.Codec, in.Candidate.Verdict, in.Candidate.BitDepth, in.Candidate.BitrateKbps = "aac", "LOSSY", 0, 256
		}, CmpKeep, "", "worse in lossless over lossy"},
		{"an UPSAMPLED incumbent gets no override", func(in *CmpInput) {
			in.Incumbent.Verdict = "UPSAMPLED"
		}, CmpKeep, "", "no better"},
		// KTD19 spectral signature
		{"same spectral signature as a FAKE incumbent never wins (different PCM)", func(in *CmpInput) {
			in.Incumbent.Verdict, in.Incumbent.TopHz = "FAKE", 16000
			in.Candidate.TopHz = 16100
		}, CmpKeep, "", "spectral signature"},
		{"signature exactly 200 Hz away still counts as the same", func(in *CmpInput) {
			in.Incumbent.Verdict, in.Incumbent.TopHz = "SUSPECT", 19000
			in.Candidate.TopHz = 19200
		}, CmpKeep, "", "spectral signature"},
		{"signature 201 Hz away is a different source", func(in *CmpInput) {
			in.Incumbent.Verdict, in.Incumbent.TopHz = "SUSPECT", 19000
			in.Candidate.TopHz = 19201
		}, CmpSwap, "", "SUSPECT"},
		{"the signature rule applies only to FAKE or SUSPECT incumbents", func(in *CmpInput) {
			in.Candidate.TopHz, in.Candidate.BitDepth = 21050, 24
		}, CmpSwap, "", "bit depth"},
		{"same PCM fingerprint never wins", func(in *CmpInput) {
			in.Candidate.Fingerprint = in.Incumbent.Fingerprint
			in.Candidate.BitDepth = 24
		}, CmpKeep, "", "exactly the same audio"},
		{"a blocked candidate never wins", func(in *CmpInput) {
			in.Blocked = true
			in.Candidate.BitDepth = 24
		}, CmpKeep, "", "blocked"},
		{"approval does not lift a block", func(in *CmpInput) {
			in.Blocked, in.Approved = true, true
			in.Candidate.BitDepth = 24
		}, CmpKeep, "", "blocked"},
		{"a candidate that failed the authenticity check never wins", func(in *CmpInput) {
			in.Candidate.Verdict = "FAKE"
			in.Candidate.BitDepth = 24
		}, CmpKeep, "", "did not pass"},
		// AE6
		{"a BLIND incumbent is not compared", func(in *CmpInput) {
			in.Incumbent.Verdict = "BLIND"
			in.Candidate.BitDepth = 24
		}, CmpKeep, "", "could not be read"},
		// AE8
		{"explicit incumbent is never replaced by a clean higher-resolution candidate", func(in *CmpInput) {
			in.Incumbent.Explicit = true
			in.Candidate.BitDepth, in.Candidate.SampleRate = 24, 96000
		}, CmpKeep, "", "explicit and clean"},
		{"clean incumbent is never replaced by an explicit candidate", func(in *CmpInput) {
			in.Candidate.Explicit = true
			in.Candidate.BitDepth = 24
		}, CmpKeep, "", "explicit and clean"},
		{"approval does not cross explicit and clean", func(in *CmpInput) {
			in.Incumbent.Explicit, in.Approved = true, true
			in.Candidate.BitDepth = 24
		}, CmpKeep, "", "explicit and clean"},
		// KTD19 mastering evidence
		{"different release group is a decision", func(in *CmpInput) {
			in.Candidate.ReleaseGroupMBID = "rg2"
			in.Candidate.BitDepth = 24
		}, CmpAsk, DecisionDifferentMaster, "different release group"},
		{"no release group on the candidate is a decision", func(in *CmpInput) {
			in.Candidate.ReleaseGroupMBID = ""
			in.Candidate.BitDepth = 24
		}, CmpAsk, DecisionDifferentMaster, "release-group evidence"},
		{"no release group on the incumbent is a decision", func(in *CmpInput) {
			in.Incumbent.ReleaseGroupMBID = ""
			in.Candidate.BitDepth = 24
		}, CmpAsk, DecisionDifferentMaster, "release-group evidence"},
		{"loudness 3 LU apart is a decision", func(in *CmpInput) {
			in.Candidate.LoudnessLUFS = cmpLufs(-12)
			in.Candidate.BitDepth = 24
		}, CmpAsk, DecisionDifferentMaster, "3.0 LU"},
		{"loudness exactly 1 LU apart is the same mastering", func(in *CmpInput) {
			in.Candidate.LoudnessLUFS = cmpLufs(-10)
			in.Candidate.BitDepth = 24
		}, CmpSwap, "", "bit depth"},
		{"missing loudness is a decision", func(in *CmpInput) {
			in.Candidate.LoudnessLUFS = nil
			in.Candidate.BitDepth = 24
		}, CmpAsk, DecisionDifferentMaster, "loudness measurement is missing"},
		{"lengths 1.5 s apart is a decision", func(in *CmpInput) {
			in.Candidate.LengthMs = 201500
			in.Candidate.BitDepth = 24
		}, CmpAsk, DecisionDifferentMaster, "decoded lengths differ"},
		{"lengths exactly 1 s apart is the same mastering", func(in *CmpInput) {
			in.Candidate.LengthMs = 201000
			in.Candidate.BitDepth = 24
		}, CmpSwap, "", "bit depth"},
		{"a missing decoded length is a decision", func(in *CmpInput) {
			in.Candidate.LengthMs = 0
			in.Candidate.BitDepth = 24
		}, CmpAsk, DecisionDifferentMaster, "length is missing"},
		{"approval turns a different master into a swap", func(in *CmpInput) {
			in.Candidate.ReleaseGroupMBID = "rg2"
			in.Candidate.BitDepth = 24
			in.Approved = true
		}, CmpSwap, "", "approved"},
		{"mastering evidence is needed even for a SUSPECT incumbent", func(in *CmpInput) {
			in.Incumbent.Verdict, in.Incumbent.TopHz = "SUSPECT", 19000
			in.Candidate.ReleaseGroupMBID = ""
		}, CmpAsk, DecisionDifferentMaster, "release-group evidence"},
		// AE15 / KTD8
		{"a lossy m4a incumbent and a passing FLAC is a path change", func(in *CmpInput) {
			in.Incumbent.Path, in.Incumbent.Codec, in.Incumbent.BitDepth, in.Incumbent.BitrateKbps = "/lib/a.m4a", "aac", 0, 256
		}, CmpAsk, DecisionPathChange, "file name and path"},
		{"approval does not lift a path change", func(in *CmpInput) {
			in.Incumbent.Path, in.Incumbent.Codec, in.Incumbent.BitDepth, in.Incumbent.BitrateKbps = "/lib/a.m4a", "aac", 0, 256
			in.Approved = true
		}, CmpAsk, DecisionPathChange, "file name and path"},
		{"extension case does not count as a path change", func(in *CmpInput) {
			in.Incumbent.Path = "/lib/a.FLAC"
			in.Candidate.BitDepth = 24
		}, CmpSwap, "", "bit depth"},
		{"lossless over lossy is an improvement in its own right", func(in *CmpInput) {
			in.Incumbent.Path, in.Incumbent.Codec, in.Incumbent.BitDepth = "/lib/a.flac", "aac", 0
			in.Incumbent.BitrateKbps = 256
		}, CmpSwap, "", "lossless over lossy"},
		{"a lossy candidate never replaces lossless", func(in *CmpInput) {
			in.Candidate.Codec, in.Candidate.Verdict, in.Candidate.BitDepth, in.Candidate.BitrateKbps = "aac", "LOSSY", 0, 256
		}, CmpKeep, "", "lossless over lossy"},
		{"higher bitrate lossy over lossy swaps", func(in *CmpInput) {
			in.Incumbent.Codec, in.Incumbent.BitDepth, in.Incumbent.BitrateKbps = "aac", 0, 128
			in.Candidate.Codec, in.Candidate.Verdict, in.Candidate.BitDepth, in.Candidate.BitrateKbps = "aac", "LOSSY", 0, 256
		}, CmpSwap, "", "bitrate"},
		// KTD16
		{"automatic swaps off turns a swap into a decision", func(in *CmpInput) {
			in.AutoSwaps = false
			in.Candidate.BitDepth = 24
		}, CmpAsk, DecisionSwapDisabled, "switched off"},
		{"automatic swaps off does not turn a keep into a decision", func(in *CmpInput) {
			in.AutoSwaps = false
		}, CmpKeep, "", "no better"},
		{"approval lifts only the swaps-off question", func(in *CmpInput) {
			in.AutoSwaps, in.Approved = false, true
			in.Candidate.BitDepth = 24
		}, CmpSwap, "", "bit depth"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := cmpIn(inc(), cand())
			tc.mutate(&in)
			got := cmpDecide(in)
			if got.Action != tc.want {
				t.Fatalf("action = %s (%q), want %s", got.Action, got.Reason, tc.want)
			}
			if got.Kind != tc.kind {
				t.Errorf("kind = %q, want %q", got.Kind, tc.kind)
			}
			if !strings.Contains(strings.ToLower(got.Reason), strings.ToLower(tc.reason)) {
				t.Errorf("reason %q should contain %q", got.Reason, tc.reason)
			}
			if got.Reason == "" {
				t.Error("every result carries a plain reason")
			}
		})
	}
}

func TestCmpBlindIncumbentIsFlaggedForRetry(t *testing.T) {
	in := cmpIn(cmpBase("/lib/a.flac", "x"), cmpBase("/stage/a.flac", "y"))
	in.Incumbent.Verdict = "BLIND"
	if got := cmpDecide(in); !got.Blind || got.Action != CmpKeep {
		t.Fatalf("got %+v, want a keep marked Blind", got)
	}
	in.Incumbent.Verdict = "SUSPECT"
	if got := cmpDecide(in); got.Blind {
		t.Fatal("a SUSPECT incumbent was treated as unreadable")
	}
}

func TestCmpExplicitHint(t *testing.T) {
	cases := map[string]int{
		"Artist - Song (Clean) [FLAC]":          -1,
		"Artist - Song [Explicit] 24-96":        1,
		"Artist - Song (Edited Version)":        -1,
		"Artist - Song FLAC":                    0,
		"Artist - Cleanse and Purify":           0,
		"Artist - Song (Explicit) (Clean Edit)": -1,
	}
	for name, want := range cases {
		if got := cmpExplicitHint(name); got != want {
			t.Errorf("cmpExplicitHint(%q) = %d, want %d", name, got, want)
		}
	}
}
