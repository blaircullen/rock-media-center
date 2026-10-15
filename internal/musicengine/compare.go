package musicengine

import (
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/tristenlammi/arrmada/internal/audiocheck"
)

// The swap comparison (R21, R22, KTD8, KTD16, KTD19). cmpDecide looks at a library file (the
// incumbent) and a checked candidate and says one of three things, with a reason a person can
// read:
//
//	swap  replace the file, nothing needs Blair
//	keep  leave the incumbent; this candidate is not an upgrade
//	ask   put it on Blair's decision list
//
// A swap needs positive evidence of "same mastering" (KTD19); anything missing is a question,
// never a guess.

// CmpAction is the outcome of a comparison.
type CmpAction int

const (
	CmpKeep CmpAction = iota
	CmpSwap
	CmpAsk
)

func (a CmpAction) String() string {
	switch a {
	case CmpSwap:
		return "swap"
	case CmpAsk:
		return "ask"
	}
	return "keep"
}

// Decision kinds a swap raises (mus_decisions.kind).
const (
	DecisionDifferentMaster = "different_master"
	DecisionTradeOff        = "trade_off"
	DecisionPathChange      = "path_change"
	DecisionSwapDisabled    = "swap_disabled"
	DecisionBlindIncumbent  = "blind_library"
)

// Thresholds from KTD19.
const (
	cmpLengthToleranceMs   = 1000 // decoded lengths must agree within 1 s
	cmpLoudnessToleranceLU = 1.0  // integrated loudness must agree within 1 LU
	cmpSignatureWindowHz   = 200  // spectral signature "the same" within 200 Hz
)

// CmpFacts is everything the comparison reads about one file.
type CmpFacts struct {
	Path             string
	Codec            string
	BitDepth         int
	SampleRate       int
	BitrateKbps      int
	Verdict          string // GENUINE, SUSPECT, FAKE, ... or LOSSY for an approved lossy candidate
	Fingerprint      string // sha256 of the decoded PCM
	TopHz            int    // spectral signature: content top
	LengthMs         int64  // decoded length
	LoudnessLUFS     *float64
	ReleaseGroupMBID string
	Explicit         bool
}

// Lossless reports whether the file's codec is lossless.
func (f CmpFacts) Lossless() bool { return isLossless(f.Codec) }

// CmpInput is one comparison.
type CmpInput struct {
	Incumbent, Candidate CmpFacts
	// Blocked says the candidate was blocked for this recording (an undone swap, or one Blair
	// kept the incumbent against).
	Blocked bool
	// AutoSwaps is the automatic-swaps setting (KTD16). Off turns a swap into a question.
	AutoSwaps bool
	// Approved is Blair's earlier yes to this very candidate: questions become swaps, but
	// every "keep" rule still holds.
	Approved bool
}

// CmpResult is the outcome and why.
type CmpResult struct {
	Action CmpAction
	// Kind is the decision kind when Action is CmpAsk.
	Kind   string
	Reason string
	// Blind is true when the incumbent could not be read, so nothing was compared. The caller
	// retries; this is never evidence that the incumbent is bad (R14).
	Blind bool
}

func cmpKeep(format string, a ...any) CmpResult {
	return CmpResult{Action: CmpKeep, Reason: fmt.Sprintf(format, a...)}
}

func cmpAsk(kind, format string, a ...any) CmpResult {
	return CmpResult{Action: CmpAsk, Kind: kind, Reason: fmt.Sprintf(format, a...)}
}

var (
	cmpCleanRe    = regexp.MustCompile(`(?i)\b(clean|censored|edited version|radio safe)\b`)
	cmpExplicitRe = regexp.MustCompile(`(?i)\bexplicit\b`)
)

// cmpExplicitHint reads a release name: +1 says explicit, -1 says clean, 0 says nothing.
func cmpExplicitHint(releaseName string) int {
	switch {
	case cmpCleanRe.MatchString(releaseName):
		return -1
	case cmpExplicitRe.MatchString(releaseName):
		return 1
	}
	return 0
}

func cmpSpectral(v string) bool {
	return v == string(audiocheck.Fake) || v == string(audiocheck.Suspect)
}

func cmpPasses(v string) bool {
	return v == string(audiocheck.Genuine) || v == "LOSSY"
}

// cmpDescribe is the short plain description used in reasons.
func cmpDescribe(f CmpFacts) string {
	if f.Lossless() {
		if f.BitDepth > 0 {
			return fmt.Sprintf("%s %d-bit/%.1f kHz", f.Codec, f.BitDepth, float64(f.SampleRate)/1000)
		}
		return fmt.Sprintf("%s %.1f kHz", f.Codec, float64(f.SampleRate)/1000)
	}
	return fmt.Sprintf("%s %d kbps (lossy)", f.Codec, f.BitrateKbps)
}

// cmpDim compares one dimension: +1 candidate better, -1 worse, 0 equal or not comparable.
func cmpDim(cand, inc int) int {
	switch {
	case cand <= 0 || inc <= 0:
		return 0
	case cand > inc:
		return 1
	case cand < inc:
		return -1
	}
	return 0
}

// cmpQuality returns how many dimensions improve and which get worse (R21).
func cmpQuality(in CmpInput) (better, worse []string) {
	note := func(d int, name string) {
		switch d {
		case 1:
			better = append(better, name)
		case -1:
			worse = append(worse, name)
		}
	}
	inc, cand := in.Incumbent, in.Candidate
	switch {
	case cand.Lossless() && !inc.Lossless():
		note(1, "lossless over lossy")
	case !cand.Lossless() && inc.Lossless():
		note(-1, "lossless over lossy")
	case cand.Lossless() && inc.Lossless():
		note(cmpDim(cand.BitDepth, inc.BitDepth), "bit depth")
		note(cmpDim(cand.SampleRate, inc.SampleRate), "sample rate")
	default:
		note(cmpDim(cand.BitrateKbps, inc.BitrateKbps), "bitrate")
	}
	return better, worse
}

// cmpMastering checks KTD19's positive evidence of the same master. It returns "" when the
// evidence is complete and agrees, else a plain reason.
func cmpMastering(inc, cand CmpFacts) string {
	switch {
	case inc.ReleaseGroupMBID == "" || cand.ReleaseGroupMBID == "":
		return "Can't tell if the new download is the same version (mastering) as the copy you have, because one of the files doesn't say which album release it came from."
	case inc.ReleaseGroupMBID != cand.ReleaseGroupMBID:
		return "The new download comes from a different album release than the copy you have, so it may be a different version (mastering)."
	case inc.LengthMs <= 0 || cand.LengthMs <= 0:
		return "Can't tell if the new download is the same version (mastering) as the copy you have, because the length of one of the files couldn't be measured."
	}
	if d := cand.LengthMs - inc.LengthMs; d > cmpLengthToleranceMs || d < -cmpLengthToleranceMs {
		return fmt.Sprintf("The two files differ in length by %.1f s (limit 1 s), so the new download may be a different edit or version (mastering).", math.Abs(float64(d))/1000)
	}
	if inc.LoudnessLUFS == nil || cand.LoudnessLUFS == nil {
		return "Can't tell if the new download is the same version (mastering) as the copy you have, because the loudness of one of the files couldn't be measured."
	}
	if d := math.Abs(*cand.LoudnessLUFS - *inc.LoudnessLUFS); d > cmpLoudnessToleranceLU {
		return fmt.Sprintf("The two files differ in loudness by %.1f LU (limit 1 LU), so the new download is probably a different version (mastering).", d)
	}
	return ""
}

// cmpDecide applies R21, R22, KTD8, KTD16 and KTD19. The order matters: rules that can never
// be overridden come first (a keep stays a keep even after Blair approves a question).
func cmpDecide(in CmpInput) CmpResult {
	inc, cand := in.Incumbent, in.Candidate

	if inc.Verdict == string(audiocheck.Blind) || inc.Verdict == "" {
		return CmpResult{Action: CmpKeep, Blind: true,
			Reason: "Couldn't read the copy you have, so nothing was compared. It will be checked again; a read failure never causes a replacement."}
	}
	if in.Blocked {
		return cmpKeep("This download is blocked for this song (the swap was undone, or you chose to keep the current file).")
	}
	if !cmpPasses(cand.Verdict) {
		return cmpKeep("The new download failed the authenticity check (%s), so it was not used.", cand.Verdict)
	}
	if inc.Fingerprint != "" && inc.Fingerprint == cand.Fingerprint {
		return cmpKeep("The new download has exactly the same audio as the copy you have, so nothing was replaced.")
	}
	if cmpSpectral(inc.Verdict) && inc.TopHz > 0 && cand.TopHz > 0 {
		if d := cand.TopHz - inc.TopHz; d <= cmpSignatureWindowHz && d >= -cmpSignatureWindowHz {
			return cmpKeep("The new download has the same %s quality problem as the copy you have (its sound cuts off at %d Hz, yours at %d Hz), so it comes from the same bad source.",
				inc.Verdict, cand.TopHz, inc.TopHz)
		}
	}
	if inc.Explicit != cand.Explicit {
		return cmpKeep("Explicit and clean versions never replace each other (the copy you have is %s, the new download is %s).",
			cmpExplicitWord(inc.Explicit), cmpExplicitWord(cand.Explicit))
	}

	// An approval waives only the question Blair was asked (mastering or trade-off). It never
	// waives an outright downgrade or the path/extension guard below, so those are evaluated
	// whether or not Blair approved.
	masteringWaived := false
	if why := cmpMastering(inc, cand); why != "" {
		if !in.Approved {
			return cmpAsk(DecisionDifferentMaster, "%s Approve to replace it, or keep the one you have.", why)
		}
		masteringWaived = true
	}

	override := cmpSpectral(inc.Verdict) && cand.Verdict == string(audiocheck.Genuine) && cand.Lossless()
	var reason string
	if override {
		reason = fmt.Sprintf("The copy you have is rated %s and the new download (%s) passes the check", inc.Verdict, cmpDescribe(cand))
	} else {
		better, worse := cmpQuality(in)
		switch {
		case len(better) > 0 && len(worse) > 0:
			if !in.Approved {
				return cmpAsk(DecisionTradeOff, "The new download (%s) is better in %s but worse in %s than the copy you have (%s). Approve to replace it, or keep the one you have.",
					cmpDescribe(cand), strings.Join(better, ", "), strings.Join(worse, ", "), cmpDescribe(inc))
			}
			reason = fmt.Sprintf("You approved the trade-off: the new download (%s) is better in %s but worse in %s than the copy you have (%s)",
				cmpDescribe(cand), strings.Join(better, ", "), strings.Join(worse, ", "), cmpDescribe(inc))
		case len(worse) > 0:
			return cmpKeep("The new download (%s) is worse in %s than the copy you have (%s), so it was not used.", cmpDescribe(cand), strings.Join(worse, ", "), cmpDescribe(inc))
		case len(better) == 0 && !masteringWaived:
			return cmpKeep("The new download (%s) is no better than the copy you have (%s), so it was not used.", cmpDescribe(cand), cmpDescribe(inc))
		case len(better) == 0:
			reason = "You approved the different version (mastering); the new download is not worse in anything measured"
		default:
			reason = fmt.Sprintf("The new download (%s) is better in %s and worse in nothing than the copy you have (%s)",
				cmpDescribe(cand), strings.Join(better, ", "), cmpDescribe(inc))
		}
	}
	if masteringWaived && !strings.HasPrefix(reason, "You approved") {
		reason = "You approved the different version (mastering). " + reason
	}

	if !strings.EqualFold(filepath.Ext(cand.Path), filepath.Ext(inc.Path)) {
		// KTD8: a swap reuses the exact path. The journal replaces a file in place, so a
		// different extension cannot be swapped in; it can only be put to Blair.
		return cmpAsk(DecisionPathChange, "%s, but replacing the %s file with a %s file would change its file name and path.",
			reason, strings.ToLower(filepath.Ext(inc.Path)), strings.ToLower(filepath.Ext(cand.Path)))
	}
	if !in.AutoSwaps && !in.Approved {
		return cmpAsk(DecisionSwapDisabled, "%s, but automatic swaps are switched off. Approve to replace it, or keep the one you have.", reason)
	}
	return CmpResult{Action: CmpSwap, Reason: reason}
}

func cmpExplicitWord(b bool) string {
	if b {
		return "explicit"
	}
	return "clean"
}
