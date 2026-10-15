// Package ytaudio is the YouTube audio lane: it finds a song's official audio upload and
// downloads the best NATIVE audio stream, as an honestly labelled lossy file. It never
// transcodes and never wraps lossy audio in FLAC (R15; "Outside this product's identity").
// The native container (.m4a, .opus, .webm) is kept exactly as YouTube serves it.
//
// The engine only calls this after Blair has approved the lossy fallback for a song.
package ytaudio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tristenlammi/arrmada/internal/toolrun"
)

// ErrNoYtDlp is returned when the yt-dlp binary is not installed.
var ErrNoYtDlp = errors.New("ytaudio: yt-dlp binary not found on PATH")

// ErrNoFfprobe is returned when ffprobe is not installed, so the real codec can't be read.
var ErrNoFfprobe = errors.New("ytaudio: ffprobe binary not found on PATH")

// Candidate is one search hit for the engine to choose from.
type Candidate struct {
	ID          string  `json:"id"`
	URL         string  `json:"url"`
	Title       string  `json:"title"`
	Channel     string  `json:"channel"`
	DurationSec float64 `json:"duration_sec"`
	// Topic is true for an auto-generated "<Artist> - Topic" channel, the label-delivered
	// official audio.
	Topic bool `json:"topic"`
	// ArtistChannel is true when the channel name matches the artist (an official upload).
	ArtistChannel bool `json:"artist_channel"`
}

// Result is a finished download. Codec and bitrate come from ffprobe on the file itself.
type Result struct {
	Path       string `json:"path"`
	SourceURL  string `json:"source_url"`
	Codec      string `json:"codec"`       // "aac" | "opus" | "vorbis" | …, as ffprobe reports it
	BitrateBPS int    `json:"bitrate_bps"` // 0 when ffprobe reports none
	Container  string `json:"container"`   // file extension without the dot
	Lossy      bool   `json:"lossy"`       // always true: YouTube serves only lossy audio
}

// RunFunc runs an external tool and returns stdout. Tests replace it; production uses toolrun.
type RunFunc func(ctx context.Context, class toolrun.Class, name string, args ...string) ([]byte, error)

// Client is the YouTube audio lane.
type Client struct {
	// Run executes tools; nil means toolrun. LookPath reports a binary's path; nil means exec.LookPath.
	Run      RunFunc
	LookPath func(string) (string, error)
}

// SearchLimit is how many search hits are requested.
const SearchLimit = 5

// SearchArgs builds the yt-dlp argv for the official-audio search. Pure.
func SearchArgs(artist, title string) []string {
	query := fmt.Sprintf("ytsearch%d:%s %s official audio", SearchLimit, artist, title)
	return []string{"--dump-json", "--flat-playlist", "--no-warnings", "--skip-download", query}
}

// DownloadArgs builds the yt-dlp argv for a native-stream download into dir. Pure. It
// deliberately has no -x, --audio-format, --recode-video, --remux-video, thumbnail embedding
// or postprocessor args: nothing may re-encode or re-wrap the stream. Plain "bestaudio" picks
// the Opus/WebM stream: on 2026-10-14 YouTube answered 403 for the m4a stream (itag 140) while
// the Opus stream downloaded, and yt-dlp does not fall back to the next format after a 403.
func DownloadArgs(url, dir string) []string {
	return []string{
		"-f", "bestaudio",
		"--no-playlist",
		"--no-warnings",
		"--no-part",
		"-o", filepath.Join(dir, "%(id)s.%(ext)s"),
		"--print", "after_move:filepath",
		"--no-simulate",
		url,
	}
}

// Search returns candidates for artist and title, best first: "- Topic" channels, then
// channels matching the artist, then the rest in YouTube's order.
func (c *Client) Search(ctx context.Context, artist, title string) ([]Candidate, error) {
	if err := c.need("yt-dlp", ErrNoYtDlp); err != nil {
		return nil, err
	}
	out, err := c.run(ctx, toolrun.Normal, "yt-dlp", SearchArgs(artist, title)...)
	if err != nil {
		return nil, fmt.Errorf("ytaudio: search %q: %w (output: %s)", artist+" - "+title, err, tail(out))
	}
	return ParseSearch(out, artist)
}

// ParseSearch parses yt-dlp's one-JSON-object-per-line search output and ranks it. Pure.
func ParseSearch(out []byte, artist string) ([]Candidate, error) {
	var cands []Candidate
	for _, line := range bytes.Split(out, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var e struct {
			ID       string  `json:"id"`
			Title    string  `json:"title"`
			URL      string  `json:"url"`
			Channel  string  `json:"channel"`
			Uploader string  `json:"uploader"`
			Duration float64 `json:"duration"`
		}
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("ytaudio: parse search line: %w (line: %.200s)", err, line)
		}
		if e.ID == "" {
			continue
		}
		ch := e.Channel
		if ch == "" {
			ch = e.Uploader
		}
		url := e.URL
		if url == "" || !strings.HasPrefix(url, "http") {
			url = "https://www.youtube.com/watch?v=" + e.ID
		}
		cands = append(cands, Candidate{
			ID: e.ID, URL: url, Title: e.Title, Channel: ch, DurationSec: e.Duration,
			Topic:         strings.HasSuffix(strings.ToLower(strings.TrimSpace(ch)), "- topic"),
			ArtistChannel: artist != "" && strings.Contains(norm(ch), norm(artist)),
		})
	}
	sort.SliceStable(cands, func(i, j int) bool { return rank(cands[i]) < rank(cands[j]) })
	return cands, nil
}

func rank(c Candidate) int {
	switch {
	case c.Topic:
		return 0
	case c.ArtistChannel:
		return 1
	}
	return 2
}

func norm(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// Download fetches url's native audio into dir (created if missing) and probes the result.
func (c *Client) Download(ctx context.Context, url, dir string) (Result, error) {
	if err := c.need("yt-dlp", ErrNoYtDlp); err != nil {
		return Result{}, err
	}
	if err := c.need("ffprobe", ErrNoFfprobe); err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, fmt.Errorf("ytaudio: create target dir %s: %w", dir, err)
	}
	out, err := c.run(ctx, toolrun.Background, "yt-dlp", DownloadArgs(url, dir)...)
	if err != nil {
		return Result{}, fmt.Errorf("ytaudio: download %s: %w (output: %s)", url, err, tail(out))
	}
	path := lastLine(out)
	if path == "" {
		return Result{}, fmt.Errorf("ytaudio: yt-dlp printed no file path for %s (output: %s)", url, tail(out))
	}
	if _, err := os.Stat(path); err != nil {
		return Result{}, fmt.Errorf("ytaudio: yt-dlp reported %s but it is not readable: %w", path, err)
	}
	probeOut, err := c.run(ctx, toolrun.Normal, "ffprobe", ProbeArgs(path)...)
	if err != nil {
		return Result{}, fmt.Errorf("ytaudio: probe %s: %w (output: %s)", path, err, tail(probeOut))
	}
	codec, bps, err := ParseProbe(probeOut)
	if err != nil {
		return Result{}, fmt.Errorf("ytaudio: probe %s: %w", path, err)
	}
	return Result{
		Path: path, SourceURL: url, Codec: codec, BitrateBPS: bps,
		Container: strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), "."), Lossy: true,
	}, nil
}

// ProbeArgs builds the ffprobe argv that reads the audio stream's codec and bitrate. Pure.
func ProbeArgs(path string) []string {
	return []string{"-v", "quiet", "-print_format", "json", "-show_format", "-show_streams", path}
}

// ParseProbe reads the first audio stream's codec and bitrate (stream bit_rate, falling
// back to the container's). Pure.
func ParseProbe(b []byte) (codec string, bitrate int, err error) {
	var o struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			BitRate   string `json:"bit_rate"`
		} `json:"streams"`
		Format struct {
			BitRate string `json:"bit_rate"`
		} `json:"format"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return "", 0, fmt.Errorf("parse ffprobe json: %w", err)
	}
	for _, s := range o.Streams {
		if s.CodecType != "audio" {
			continue
		}
		bps, _ := strconv.Atoi(s.BitRate)
		if bps == 0 {
			bps, _ = strconv.Atoi(o.Format.BitRate)
		}
		return strings.ToLower(s.CodecName), bps, nil
	}
	return "", 0, errors.New("no audio stream in file")
}

func (c *Client) need(bin string, missing error) error {
	look := c.LookPath
	if look == nil {
		look = exec.LookPath
	}
	if _, err := look(bin); err != nil {
		return fmt.Errorf("%w: %v", missing, err)
	}
	return nil
}

func (c *Client) run(ctx context.Context, class toolrun.Class, name string, args ...string) ([]byte, error) {
	if c.Run != nil {
		return c.Run(ctx, class, name, args...)
	}
	// A hard deadline so a stuck yt-dlp can't hang the lane.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	return toolrun.Command(ctx, class, name, args...).CombinedOutput()
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// tail keeps the last part of tool output for error messages.
func tail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 400 {
		s = "…" + s[len(s)-400:]
	}
	return s
}
