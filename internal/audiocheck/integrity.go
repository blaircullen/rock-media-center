package audiocheck

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// pcmBytesPerSample is the width of the s32le the decode pass writes. s32le keeps every
// source depth lossless (16 and 24-bit samples are left-aligned) and makes the same audio
// hash the same whether it came from FLAC, WAV or ALAC.
const pcmBytesPerSample = 4

// Fingerprint returns the sha256 of the file's decoded PCM (R25). Unlike Check it returns
// an error when the file cannot be fully decoded.
func Fingerprint(ctx context.Context, path string) (string, error) {
	c := New()
	f, err := c.probe(ctx, path)
	if err != nil {
		return "", err
	}
	integ, fp, err := c.decodeAll(ctx, path, f)
	if err != nil {
		return "", err
	}
	if !integ.OK {
		return "", errors.New(integ.Detail)
	}
	return fp, nil
}

// decodeAll decodes the whole first audio stream to null-equivalent s32le, hashing it. One
// pass answers two questions: did every frame decode (decoder stderr is empty, decoded length
// matches the probe) and what is the audio's fingerprint.
//
// A non-nil error means the pass could not look (deadline, tool failure, I/O error, nothing
// decoded) and the caller reports BLIND. A returned Integrity with OK=false means it looked
// and the file is damaged.
func (c *Checker) decodeAll(ctx context.Context, path string, f Facts) (Integrity, string, error) {
	h := newPCMHasher()
	stderr, err := c.call(ctx, fullPassTimeout, c.Class, h, "ffmpeg",
		"-v", "error", "-i", path, "-map", "0:a:0", "-f", "s32le", "-")
	text := strings.TrimSpace(string(stderr))
	if err != nil {
		if ctx.Err() != nil || isDeadline(err) || looksLikeIOError(text) || !isExit(err) {
			return Integrity{}, "", err
		}
		// ffmpeg ran, looked, and rejected the stream: damaged file.
		return Integrity{Detail: "decoder failed: " + tail(stderr)}, "", nil
	}
	if h.n == 0 {
		return Integrity{}, "", errors.New("empty decode - unreadable or truncated")
	}
	decoded := float64(h.n) / float64(f.Channels*f.SampleRate*pcmBytesPerSample)
	integ := Integrity{OK: true, DecodedSec: decoded}
	switch {
	case text != "":
		if looksLikeIOError(text) {
			return Integrity{}, "", fmt.Errorf("i/o error during decode: %s", tail(stderr))
		}
		integ.OK, integ.Detail = false, "decoder reported errors: "+tail(stderr)
	case absf(decoded-f.DurationSec) > lengthTolerance(f.Codec):
		integ.OK = false
		integ.Detail = fmt.Sprintf("decoded %.2fs but the file reports %.2fs (truncated)", decoded, f.DurationSec)
	}
	return integ, h.sum(), nil
}

// lengthTolerance is how far the decoded length may stray from the probed duration.
func lengthTolerance(codec string) float64 {
	switch {
	case codec == "flac", codec == "alac", codec == "wavpack", codec == "ape", strings.HasPrefix(codec, "pcm_"):
		return losslessLengthToleranceSec
	}
	return lossyLengthToleranceSec
}

// ioMarkers are decoder/OS messages that mean the storage failed, not the audio.
var ioMarkers = []string{
	"input/output error", "i/o error", "stale file handle", "connection timed out",
	"transport endpoint", "resource temporarily unavailable", "permission denied",
	"no such file", "device not configured", "host is down", "network is unreachable",
}

func looksLikeIOError(stderr string) bool {
	s := strings.ToLower(stderr)
	for _, m := range ioMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// isDeadline reports a tool killed by its own deadline (the error text carries the context error).
func isDeadline(err error) bool { return errors.Is(err, context.DeadlineExceeded) }

// isExit reports that the tool started and exited non-zero, as opposed to failing to start.
func isExit(err error) bool {
	var ee interface{ ExitCode() int }
	return errors.As(err, &ee)
}
