package musicengine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tristenlammi/arrmada/internal/toolrun"
)

// FFprobeTags reads file tags with ffprobe, started through internal/toolrun like every
// other helper tool.
type FFprobeTags struct {
	// Timeout bounds one read; 0 means 30 s.
	Timeout time.Duration
}

var _ TagReader = FFprobeTags{}

const defaultTagTimeout = 30 * time.Second

// ReadTags runs ffprobe. Any failure, including a tool exit and I/O errors from the share,
// is an error carrying ffprobe's own message.
func (f FFprobeTags) ReadTags(ctx context.Context, path string) (Tags, error) {
	to := f.Timeout
	if to <= 0 {
		to = defaultTagTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	cmd := toolrun.Command(ctx, toolrun.Normal, "ffprobe",
		"-v", "error", "-print_format", "json", "-show_entries", "format=duration:format_tags", path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return Tags{}, fmt.Errorf("ffprobe tags of %s: %w: %s", path, err, clip(stderr.String(), 300))
	}
	return parseTags(stdout.Bytes())
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// parseTags reads ffprobe's -show_entries format=duration:format_tags JSON. Tag names differ
// by container (FLAC MUSICBRAINZ_TRACKID, MP4 "MusicBrainz Track Id"), so keys are compared
// lower-cased with spaces, underscores and dashes removed. Picard calls the recording id
// "trackid"; "releasetrackid" is a different id and is not read.
func parseTags(b []byte) (Tags, error) {
	var out struct {
		Format struct {
			Duration string            `json:"duration"`
			Tags     map[string]string `json:"tags"`
		} `json:"format"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return Tags{}, fmt.Errorf("unreadable ffprobe output: %w", err)
	}
	norm := map[string]string{}
	for k, v := range out.Format.Tags {
		k = strings.NewReplacer(" ", "", "_", "", "-", "").Replace(strings.ToLower(k))
		norm[k] = strings.TrimSpace(v)
	}
	var t Tags
	for _, k := range []string{"musicbrainztrackid", "musicbrainzrecordingid"} {
		if v := norm[k]; uuidRe.MatchString(v) {
			t.RecordingMBID = strings.ToLower(v)
			break
		}
	}
	if v := norm["musicbrainzalbumid"]; uuidRe.MatchString(v) {
		t.ReleaseMBID = strings.ToLower(v)
	}
	t.ISRC = strings.ToUpper(strings.ReplaceAll(norm["isrc"], "-", ""))
	t.Title, t.Artist, t.Album = norm["title"], norm["artist"], norm["album"]
	if t.Artist == "" {
		t.Artist = norm["albumartist"]
	}
	if d, err := strconv.ParseFloat(out.Format.Duration, 64); err == nil {
		t.DurationSec = d
	}
	return t, nil
}
