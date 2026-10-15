package musicengine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strings"

	"github.com/tristenlammi/arrmada/internal/toolrun"
)

// This file holds the acquirer's proof that a downloaded file is the pinned recording and
// that its codec is lossless (Astra findings 2, 4, 11). It reads the file's own tags; it
// never takes a release id from the requested recording.

// acqLengthToleranceSec is how far a file's length may be from the pinned recording's length
// before the file counts as a different version. The plan gives no number for acquisition
// (KTD19's 1 s is for comparing two copies of one master); remasters and fades differ by a
// few seconds, a live or extended cut by more.
const acqLengthToleranceSec = 5.0

// AcquireFileTags are the tags the acquirer reads from a downloaded file.
type AcquireFileTags struct {
	RecordingMBID    string // MUSICBRAINZ_TRACKID / MUSICBRAINZ_RECORDINGID
	ReleaseMBID      string // MUSICBRAINZ_ALBUMID
	ReleaseGroupMBID string // MUSICBRAINZ_RELEASEGROUPID
	Title            string
	DurationSec      float64
	// Advisory is "explicit", "clean" or "" (no content-advisory tag).
	Advisory string
}

// AcquireTagReader reads the tags of one file. An unreadable file is an error, never empty
// tags.
type AcquireTagReader func(ctx context.Context, path string) (AcquireFileTags, error)

// ffprobeAcquireTags is the production AcquireTagReader: ffprobe through internal/toolrun.
func ffprobeAcquireTags(ctx context.Context, path string) (AcquireFileTags, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultTagTimeout)
	defer cancel()
	cmd := toolrun.Command(ctx, toolrun.Normal, "ffprobe",
		"-v", "error", "-print_format", "json", "-show_entries", "format=duration:format_tags", path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return AcquireFileTags{}, fmt.Errorf("ffprobe tags of %s: %w: %s", path, err, clip(stderr.String(), 300))
	}
	return parseAcquireTags(stdout.Bytes())
}

// parseAcquireTags reads ffprobe's format=duration:format_tags JSON. It reuses parseTags for
// the ids it already reads and adds the release group id and the content advisory.
func parseAcquireTags(b []byte) (AcquireFileTags, error) {
	base, err := parseTags(b)
	if err != nil {
		return AcquireFileTags{}, err
	}
	var raw struct {
		Format struct {
			Tags map[string]string `json:"tags"`
		} `json:"format"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return AcquireFileTags{}, fmt.Errorf("unreadable ffprobe output: %w", err)
	}
	norm := map[string]string{}
	for k, v := range raw.Format.Tags {
		k = strings.NewReplacer(" ", "", "_", "", "-", "").Replace(strings.ToLower(k))
		norm[k] = strings.TrimSpace(v)
	}
	out := AcquireFileTags{
		RecordingMBID: base.RecordingMBID, ReleaseMBID: base.ReleaseMBID,
		Title: base.Title, DurationSec: base.DurationSec,
	}
	if v := norm["musicbrainzreleasegroupid"]; uuidRe.MatchString(v) {
		out.ReleaseGroupMBID = strings.ToLower(v)
	}
	// iTunes content advisory (rtng): 1 and 4 are explicit, 2 is clean.
	switch norm["itunesadvisory"] {
	case "1", "4":
		out.Advisory = "explicit"
	case "2":
		out.Advisory = "clean"
	}
	return out, nil
}

// acqWantedVersionWords are the version words the pinned recording records ("studio" is the
// absence of any).
func acqWantedVersionWords(hint string) []string {
	var out []string
	for _, w := range strings.Split(hint, ",") {
		w = acqNorm(w)
		if w != "" && w != "studio" {
			out = append(out, w)
		}
	}
	return out
}

// acqCheckIdentity decides whether the checked file is the pinned recording. reject is
// non-empty when a fact contradicts it. note is non-empty when nothing contradicts it but
// something could not be proved; the caller keeps its current behaviour and records the note.
// probedSec is the decoded/probed length of the file (0 when unknown).
func acqCheckIdentity(rec Recording, c candRow, target string, tags AcquireFileTags, tagsErr error, probedSec float64) (reject, note string) {
	var unproven []string
	tagMatch := false
	switch {
	case tagsErr != nil:
		unproven = append(unproven, "the file's tags couldn't be read ("+clip(tagsErr.Error(), 120)+")")
	case tags.RecordingMBID == "":
		unproven = append(unproven, "the file has no song-ID tag")
	case rec.MBID == "":
		unproven = append(unproven, "the requested song has no ID on record")
	case !strings.EqualFold(tags.RecordingMBID, rec.MBID):
		return fmt.Sprintf("Wrong song: the file is tagged as recording %s, but the song you asked for is %s.", tags.RecordingMBID, rec.MBID), ""
	default:
		tagMatch = true
	}

	if tagsErr == nil {
		// Explicit/clean mismatch is a different release of the song.
		if tags.Advisory == "explicit" && !rec.Explicit {
			return "The file is marked explicit, but the song you asked for is the clean version.", ""
		}
		if tags.Advisory == "clean" && rec.Explicit {
			return "The file is marked clean, but the song you asked for is the explicit version.", ""
		}
	}

	if !tagMatch {
		// Without a matching recording id the names have to say it is the right version.
		hay := " " + acqNorm(c.ReleaseName+" "+filepath.Base(target)+" "+tags.Title) + " "
		for _, w := range acqWantedVersionWords(rec.VersionHint) {
			if !strings.Contains(hay, " "+w+" ") {
				return fmt.Sprintf("You asked for the %s version, but neither the release name nor the file says it is.", w), ""
			}
		}
		if tags.Title != "" {
			allowed := " " + acqNorm(rec.VersionHint) + " " + acqNorm(rec.Title) + " "
			title := " " + acqNorm(tags.Title) + " "
			for _, v := range stagingVersionWords {
				if strings.Contains(title, " "+v+" ") && !strings.Contains(allowed, " "+v+" ") {
					return fmt.Sprintf("The file's title %q names a %s version, which is not the one you asked for.", tags.Title, v), ""
				}
			}
		}

		got := probedSec
		if got <= 0 {
			got = tags.DurationSec
		}
		switch {
		case rec.LengthMs <= 0:
			unproven = append(unproven, "the requested song's length isn't on record")
		case got <= 0:
			unproven = append(unproven, "the file's length is unknown")
		default:
			want := float64(rec.LengthMs) / 1000
			if d := math.Abs(got - want); d > acqLengthToleranceSec {
				return fmt.Sprintf("Wrong length: the file runs %.1fs, the song you asked for runs %.1fs (allowed difference %.0fs).", got, want, acqLengthToleranceSec), ""
			}
		}
	}
	if len(unproven) > 0 {
		return "", "Couldn't fully confirm this is the right song: " + strings.Join(unproven, "; ")
	}
	return "", ""
}
