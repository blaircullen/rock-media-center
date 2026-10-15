package audiocheck

import (
	"context"
	"testing"
)

func TestFactsOf24Bit96kFixture(t *testing.T) {
	p := fixture(t, "hires.flac", func(t *testing.T, out string) {
		buildFlac(t, out, noise(10, 96000, 0.5), "", 24, 2)
	})
	f, err := Probe(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if f.BitDepth != 24 || f.SampleRate != 96000 || f.Channels != 2 || f.Codec != "flac" {
		t.Errorf("facts = %+v, want flac 24-bit 96000 Hz stereo", f)
	}
	if f.BitrateKbps <= 0 || f.SizeBytes <= 0 || f.DurationSec < 9.9 || f.DurationSec > 10.1 {
		t.Errorf("facts = %+v, want a nonzero bitrate and size and ~10s", f)
	}
}

func TestParseProbe(t *testing.T) {
	b := []byte(`{"streams":[
		{"codec_type":"video","codec_name":"mjpeg","disposition":{"attached_pic":1}},
		{"codec_type":"audio","codec_name":"flac","sample_fmt":"s32","sample_rate":"96000","channels":2,
		 "bits_per_sample":0,"bits_per_raw_sample":"24","disposition":{"attached_pic":0}}],
		"format":{"format_name":"flac","duration":"10.0","size":"1250000","bit_rate":"1000000"}}`)
	f, err := parseProbe(b)
	if err != nil {
		t.Fatal(err)
	}
	if f.BitDepth != 24 || f.SampleRate != 96000 || f.BitrateKbps != 1000 || f.Container != "flac" {
		t.Errorf("facts = %+v", f)
	}
}

func TestParseProbeRejectsUnusableFiles(t *testing.T) {
	for name, in := range map[string]string{
		"no audio":    `{"streams":[{"codec_type":"video"}],"format":{"duration":"5"}}`,
		"no duration": `{"streams":[{"codec_type":"audio","codec_name":"flac","sample_rate":"44100","channels":2}],"format":{}}`,
		"not json":    `oops`,
	} {
		if _, err := parseProbe([]byte(in)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestParseIntegratedLoudness(t *testing.T) {
	out := []byte("[Parsed_ebur128_0 @ 0x1] Summary:\n\n  Integrated loudness:\n    I:         -14.2 LUFS\n    Threshold: -24.2 LUFS\n\n  Loudness range:\n    LRA:         3.1 LU\n")
	v, err := parseIntegratedLoudness(out)
	if err != nil || v != -14.2 {
		t.Errorf("got %v, %v; want -14.2", v, err)
	}
	if _, err := parseIntegratedLoudness([]byte("    I:         -inf LUFS\n")); err == nil {
		t.Error("-inf must be an error")
	}
	if _, err := parseIntegratedLoudness([]byte("nothing")); err == nil {
		t.Error("missing summary must be an error")
	}
}
