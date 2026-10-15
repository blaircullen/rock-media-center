package audiocheck

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strconv"
)

// Loudness returns EBU R128 integrated loudness in LUFS (ffmpeg ebur128 filter).
func Loudness(ctx context.Context, path string) (float64, error) { return New().loudness(ctx, path) }

// The ebur128 summary prints "    I:         -14.2 LUFS" under "Integrated loudness:".
var integratedRE = regexp.MustCompile(`(?m)^\s*I:\s+(-?inf|-?[0-9]+(?:\.[0-9]+)?)\s+LUFS`)

func (c *Checker) loudness(ctx context.Context, path string) (float64, error) {
	var sink bytes.Buffer
	stderr, err := c.call(ctx, fullPassTimeout, c.Class, &sink, "ffmpeg",
		"-hide_banner", "-nostats", "-i", path, "-map", "0:a:0",
		"-af", "ebur128=framelog=quiet", "-f", "null", "-")
	if err != nil {
		return 0, err
	}
	return parseIntegratedLoudness(stderr)
}

func parseIntegratedLoudness(stderr []byte) (float64, error) {
	m := integratedRE.FindAllSubmatch(stderr, -1)
	if len(m) == 0 {
		return 0, fmt.Errorf("no integrated loudness in ffmpeg output: %s", tail(stderr))
	}
	last := string(m[len(m)-1][1])
	v, err := strconv.ParseFloat(last, 64)
	if err != nil || last == "-inf" {
		return 0, fmt.Errorf("integrated loudness is %q (silent or too short to measure)", last)
	}
	return v, nil
}
