package audiocheck

import (
	"bytes"
	"context"
	"fmt"
	"strings"
)

// checksLSB reports whether the bit-depth check applies: integer PCM wider than 16 bits.
// Float containers have no integer LSBs to inspect.
func checksLSB(f Facts) bool {
	if f.BitDepth <= 16 {
		return false
	}
	return !strings.HasPrefix(f.SampleFmt, "flt") && !strings.HasPrefix(f.SampleFmt, "dbl")
}

// bitDepth reads the least-significant bits in windows across the whole file (KTD5):
// true LSBs active in any window means the depth is genuine. All windows read and all zero
// means a padded container. If some window could not be read and none was active, the
// zeros prove nothing, so the check is an error (BLIND).
//
// ffmpeg's s32le left-aligns samples wider than 16 bits (24-bit audio sits in bits 8-31),
// so a low byte read from s32le is always zero. s24le carries the sample as three
// little-endian bytes whose first byte is the true LSB; 32-bit audio is native in s32le.
func (c *Checker) bitDepth(ctx context.Context, path string, f Facts) (BitDepth, error) {
	format, stride := "s24le", 3
	if f.BitDepth >= 32 {
		format, stride = "s32le", 4
	}
	bd := BitDepth{Bits: f.BitDepth}
	var firstErr error
	for _, w := range lsbWindowStarts(f.DurationSec) {
		args := []string{"-v", "error"}
		if w.length > 0 {
			args = append(args, "-ss", fmt.Sprintf("%.2f", w.start), "-t", fmt.Sprintf("%.2f", w.length))
		}
		args = append(args, "-i", path, "-map", "0:a:0", "-f", format, "-")
		var out bytes.Buffer
		if _, err := c.call(ctx, analysisTimeout, c.Class, &out, "ffmpeg", args...); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		raw := out.Bytes()
		if len(raw) < stride {
			if firstErr == nil {
				firstErr = fmt.Errorf("empty low-bit read at %.1fs", w.start)
			}
			continue
		}
		bd.WindowsRead++
		active := false
		for i := 0; i+stride <= len(raw); i += stride {
			if raw[i] != 0 {
				active = true
				break
			}
		}
		if active {
			bd.WindowsActive++
			bd.LSBActive = true
			return bd, nil // one active window settles it
		}
	}
	if firstErr != nil {
		return bd, fmt.Errorf("%d window(s) read all-zero, at least one unreadable: %w", bd.WindowsRead, firstErr)
	}
	return bd, nil
}

type lsbWindow struct{ start, length float64 }

// lsbWindowStarts spreads lsbWindows windows evenly from the start to the end of the file.
// A file no longer than all windows together is read once in full (length 0 = no -t).
func lsbWindowStarts(dur float64) []lsbWindow {
	if dur <= lsbWindows*lsbWindowSeconds {
		return []lsbWindow{{0, 0}}
	}
	ws := make([]lsbWindow, lsbWindows)
	span := dur - lsbWindowSeconds
	for i := range ws {
		ws[i] = lsbWindow{span * float64(i) / float64(lsbWindows-1), lsbWindowSeconds}
	}
	return ws
}
