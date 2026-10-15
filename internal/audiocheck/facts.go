package audiocheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/tristenlammi/arrmada/internal/toolrun"
)

// Probe reads a file's real properties with ffprobe (R16).
func Probe(ctx context.Context, path string) (Facts, error) { return New().probe(ctx, path) }

// flexInt reads an ffprobe number that is a JSON string in some fields
// ("bits_per_raw_sample": "24") and a JSON number in others ("bits_per_sample": 16).
type flexInt int64

func (n *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" || s == "N/A" {
		*n = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("not an integer: %q", s)
	}
	*n = flexInt(v)
	return nil
}

type probeOut struct {
	Streams []struct {
		CodecType        string  `json:"codec_type"`
		CodecName        string  `json:"codec_name"`
		SampleFmt        string  `json:"sample_fmt"`
		SampleRate       flexInt `json:"sample_rate"`
		Channels         flexInt `json:"channels"`
		BitsPerSample    flexInt `json:"bits_per_sample"`
		BitsPerRawSample flexInt `json:"bits_per_raw_sample"`
		Disposition      struct {
			AttachedPic int `json:"attached_pic"`
		} `json:"disposition"`
	} `json:"streams"`
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
		Size       string `json:"size"`
		BitRate    string `json:"bit_rate"`
	} `json:"format"`
}

func (c *Checker) probe(ctx context.Context, path string) (Facts, error) {
	var out bytes.Buffer
	_, err := c.call(ctx, probeTimeout, toolrun.Normal, &out, "ffprobe",
		"-v", "error", "-print_format", "json", "-show_streams", "-show_format", path)
	if err != nil {
		return Facts{}, err
	}
	return parseProbe(out.Bytes())
}

func parseProbe(b []byte) (Facts, error) {
	var o probeOut
	if err := json.Unmarshal(b, &o); err != nil {
		return Facts{}, fmt.Errorf("unreadable ffprobe output: %w", err)
	}
	for _, s := range o.Streams {
		if s.CodecType != "audio" || s.Disposition.AttachedPic == 1 {
			continue
		}
		f := Facts{
			Codec:      s.CodecName,
			Container:  o.Format.FormatName,
			SampleFmt:  s.SampleFmt,
			BitDepth:   int(s.BitsPerRawSample),
			SampleRate: int(s.SampleRate),
			Channels:   int(s.Channels),
		}
		if f.BitDepth == 0 {
			f.BitDepth = int(s.BitsPerSample)
		}
		f.DurationSec, _ = strconv.ParseFloat(o.Format.Duration, 64)
		f.SizeBytes, _ = strconv.ParseInt(o.Format.Size, 10, 64)
		if f.DurationSec > 0 && f.SizeBytes > 0 {
			f.BitrateKbps = float64(f.SizeBytes) * 8 / f.DurationSec / 1000
		} else if br, err := strconv.ParseFloat(o.Format.BitRate, 64); err == nil {
			f.BitrateKbps = br / 1000
		}
		if f.SampleRate <= 0 || f.Channels <= 0 {
			return Facts{}, errors.New("audio stream has no sample rate or channel count")
		}
		if f.DurationSec <= 0 {
			return Facts{}, errors.New("file reports no duration")
		}
		return f, nil
	}
	return Facts{}, errors.New("no audio stream")
}
