package ytaudio

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tristenlammi/arrmada/internal/toolrun"
)

func TestDownloadArgsKeepNativeAudio(t *testing.T) {
	args := DownloadArgs("https://www.youtube.com/watch?v=abc", "/tmp/x")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-f bestaudio ") {
		t.Fatalf("missing native format selector: %v", args)
	}
	banned := []string{"-x", "--extract-audio", "--audio-format", "--audio-quality", "--recode-video",
		"--remux-video", "--postprocessor-args", "--ppa", "--embed-thumbnail", "--convert-thumbnails",
		"--merge-output-format", "--exec"}
	for _, a := range args {
		for _, b := range banned {
			if a == b {
				t.Errorf("argv contains forbidden option %q: %v", b, args)
			}
		}
		if strings.Contains(strings.ToLower(a), "flac") {
			t.Errorf("argv mentions flac: %q", a)
		}
	}
}

func TestSearchArgs(t *testing.T) {
	args := SearchArgs("Boston", "More Than a Feeling")
	last := args[len(args)-1]
	if last != "ytsearch5:Boston More Than a Feeling official audio" {
		t.Fatalf("query = %q", last)
	}
}

func TestMissingYtDlpIsAnError(t *testing.T) {
	c := &Client{LookPath: func(string) (string, error) { return "", errors.New("not found") }}
	if _, err := c.Search(context.Background(), "a", "b"); !errors.Is(err, ErrNoYtDlp) {
		t.Fatalf("Search err = %v, want ErrNoYtDlp", err)
	}
	res, err := c.Download(context.Background(), "u", t.TempDir())
	if !errors.Is(err, ErrNoYtDlp) || res.Path != "" {
		t.Fatalf("Download = %+v, %v", res, err)
	}
}

func TestSearchRanksTopicThenArtistChannel(t *testing.T) {
	out := `{"id":"a1","title":"Song (lyrics)","channel":"Random Lyrics","duration":200,"url":"https://www.youtube.com/watch?v=a1"}
{"id":"b2","title":"Song","channel":"Boston","duration":205}
{"id":"c3","title":"Song","channel":"Boston - Topic","duration":204}
`
	c := &Client{
		LookPath: func(string) (string, error) { return "/x", nil },
		Run:      func(context.Context, toolrun.Class, string, ...string) ([]byte, error) { return []byte(out), nil },
	}
	got, err := c.Search(context.Background(), "Boston", "Song")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != "c3" || got[1].ID != "b2" || got[2].ID != "a1" {
		t.Fatalf("order = %+v", got)
	}
	if !got[0].Topic || !got[1].ArtistChannel || got[1].URL != "https://www.youtube.com/watch?v=b2" {
		t.Fatalf("flags/url wrong: %+v", got)
	}
}

func TestDownloadLabelsOpus(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "abc.webm")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var ytArgs []string
	c := &Client{
		LookPath: func(string) (string, error) { return "/x", nil },
		Run: func(_ context.Context, _ toolrun.Class, name string, args ...string) ([]byte, error) {
			if name == "yt-dlp" {
				ytArgs = args
				return []byte(file + "\n"), nil
			}
			return []byte(`{"streams":[{"codec_type":"audio","codec_name":"opus","bit_rate":"131072"}],"format":{"bit_rate":"140000"}}`), nil
		},
	}
	res, err := c.Download(context.Background(), "https://youtu.be/abc", dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Codec != "opus" || res.BitrateBPS != 131072 || res.Container != "webm" || !res.Lossy || res.SourceURL != "https://youtu.be/abc" {
		t.Fatalf("result = %+v", res)
	}
	for _, a := range ytArgs {
		if a == "-x" || a == "--audio-format" {
			t.Fatalf("download argv re-encodes: %v", ytArgs)
		}
	}
}

func TestParseProbeFallsBackToFormatBitrate(t *testing.T) {
	codec, bps, err := ParseProbe([]byte(`{"streams":[{"codec_type":"audio","codec_name":"AAC"}],"format":{"bit_rate":"129000"}}`))
	if err != nil || codec != "aac" || bps != 129000 {
		t.Fatalf("got %q %d %v", codec, bps, err)
	}
	if _, _, err := ParseProbe([]byte(`{"streams":[{"codec_type":"video","codec_name":"h264"}]}`)); err == nil {
		t.Fatal("want error for no audio stream")
	}
}
