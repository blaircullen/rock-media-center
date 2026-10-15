package metadata

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// MusicRecordings is the recording-level MusicBrainz surface the music engine needs to pin a
// request to the exact recording the listener heard (R6). It is deliberately separate from
// MusicProvider so that interface, and its fakes, stay as they are (KTD15).
//
// Pick the pacer class with WithMBClass on the context; the default is catalogue.
type MusicRecordings interface {
	// RecordingsByISRC lists the recordings registered under an ISRC, each with its releases.
	RecordingsByISRC(ctx context.Context, isrc string) ([]Recording, error)
	// GetRecording fetches one recording with its releases, release groups and ISRCs.
	GetRecording(ctx context.Context, mbid string) (*Recording, error)
	// GetRelease fetches a release with its media and tracks.
	GetRelease(ctx context.Context, mbid string) (*ReleaseDetails, error)
	// SearchRecordings is the no-ISRC path: artist + title, narrowed by duration when
	// durationMs > 0. Results are ordered best match first.
	SearchRecordings(ctx context.Context, artist, title string, durationMs int) ([]Recording, error)
}

var _ MusicRecordings = (*MusicBrainz)(nil)

// Recording is one MusicBrainz recording.
type Recording struct {
	MBID   string `json:"mbid"`
	Title  string `json:"title"`
	Artist string `json:"artist,omitempty"` // joined artist credit
	// ArtistMBIDs are the credited artists' ids, in credit order.
	ArtistMBIDs []string `json:"artist_mbids,omitempty"`
	LengthMs    int      `json:"length_ms,omitempty"`
	// Disambiguation is MusicBrainz's qualifier ("live at Wembley", "acoustic version").
	Disambiguation string `json:"disambiguation,omitempty"`
	// Versions are hints derived from the disambiguation: live, acoustic, remix, edit, demo.
	Versions []string     `json:"versions,omitempty"`
	ISRCs    []string     `json:"isrcs,omitempty"`
	Releases []ReleaseRef `json:"releases,omitempty"`
	// Score is the search relevance (0-100); zero outside search results.
	Score int `json:"score,omitempty"`
}

// ReleaseRef is a release a recording appears on, with its release group.
type ReleaseRef struct {
	MBID              string   `json:"mbid"`
	Title             string   `json:"title"`
	Status            string   `json:"status,omitempty"`
	Date              string   `json:"date,omitempty"`
	Country           string   `json:"country,omitempty"`
	ReleaseGroupMBID  string   `json:"release_group_mbid,omitempty"`
	ReleaseGroupTitle string   `json:"release_group_title,omitempty"`
	PrimaryType       string   `json:"primary_type,omitempty"`
	SecondaryTypes    []string `json:"secondary_types,omitempty"`
	// Versions are hints from the release group's secondary types (Live, Remix, Demo).
	Versions []string `json:"versions,omitempty"`
}

// ReleaseDetails is a release with its track listing.
type ReleaseDetails struct {
	ReleaseRef
	Artist string         `json:"artist,omitempty"`
	Tracks []ReleaseTrack `json:"tracks"`
}

// ReleaseTrack is one track on a release.
type ReleaseTrack struct {
	RecordingMBID  string   `json:"recording_mbid"`
	Title          string   `json:"title"`
	Disc           int      `json:"disc"`
	Position       int      `json:"position"`
	LengthMs       int      `json:"length_ms,omitempty"`
	Disambiguation string   `json:"disambiguation,omitempty"`
	Versions       []string `json:"versions,omitempty"`
}

// ---- wire shapes ----

type mbArtistCredit struct {
	Name       string `json:"name"`
	JoinPhrase string `json:"joinphrase"`
	Artist     struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"artist"`
}

type mbRelease struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Status       string `json:"status"`
	Date         string `json:"date"`
	Country      string `json:"country"`
	ReleaseGroup struct {
		ID             string   `json:"id"`
		Title          string   `json:"title"`
		PrimaryType    string   `json:"primary-type"`
		SecondaryTypes []string `json:"secondary-types"`
	} `json:"release-group"`
}

type mbRecording struct {
	ID             string           `json:"id"`
	Title          string           `json:"title"`
	Length         int              `json:"length"`
	Disambiguation string           `json:"disambiguation"`
	ISRCs          []string         `json:"isrcs"`
	ArtistCredit   []mbArtistCredit `json:"artist-credit"`
	Releases       []mbRelease      `json:"releases"`
	Score          int              `json:"score"`
}

func (r mbRecording) toRecording() Recording {
	artist, ids := mbJoinCredit(r.ArtistCredit)
	out := Recording{
		MBID: r.ID, Title: r.Title, Artist: artist, ArtistMBIDs: ids,
		LengthMs: r.Length, Disambiguation: r.Disambiguation,
		Versions: mbVersionHints(r.Disambiguation), ISRCs: r.ISRCs, Score: r.Score,
	}
	for _, rel := range r.Releases {
		out.Releases = append(out.Releases, rel.toRef())
	}
	return out
}

func (r mbRelease) toRef() ReleaseRef {
	return ReleaseRef{
		MBID: r.ID, Title: r.Title, Status: r.Status, Date: r.Date, Country: r.Country,
		ReleaseGroupMBID: r.ReleaseGroup.ID, ReleaseGroupTitle: r.ReleaseGroup.Title,
		PrimaryType: r.ReleaseGroup.PrimaryType, SecondaryTypes: r.ReleaseGroup.SecondaryTypes,
		Versions: mbSecondaryTypeHints(r.ReleaseGroup.SecondaryTypes),
	}
}

func mbJoinCredit(credits []mbArtistCredit) (string, []string) {
	var sb strings.Builder
	var ids []string
	for _, c := range credits {
		name := c.Name
		if name == "" {
			name = c.Artist.Name
		}
		sb.WriteString(name)
		sb.WriteString(c.JoinPhrase)
		if c.Artist.ID != "" {
			ids = append(ids, c.Artist.ID)
		}
	}
	return strings.TrimSpace(sb.String()), ids
}

// ---- version hints ----

var mbVersionWords = regexp.MustCompile(`\b(live|acoustic|remix(?:ed)?|edit(?:ed)?|demo)\b`)

// mbVersionHints reads live / acoustic / remix / edit / demo out of a recording
// disambiguation. These are hints for the caller to weigh, not a verdict.
func mbVersionHints(disambiguation string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range mbVersionWords.FindAllString(strings.ToLower(disambiguation), -1) {
		var h string
		switch m {
		case "remixed":
			h = "remix"
		case "edited":
			h = "edit"
		default:
			h = m
		}
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// mbSecondaryTypeHints maps release-group secondary types onto the same hint vocabulary.
func mbSecondaryTypeHints(types []string) []string {
	var out []string
	for _, t := range types {
		switch strings.ToLower(t) {
		case "live":
			out = append(out, "live")
		case "remix":
			out = append(out, "remix")
		case "demo":
			out = append(out, "demo")
		}
	}
	return out
}

// ---- lookups ----

const mbRecordingInc = "releases+release-groups+artist-credits"

// RecordingsByISRC implements MusicRecordings.
func (m *MusicBrainz) RecordingsByISRC(ctx context.Context, isrc string) ([]Recording, error) {
	isrc = strings.ToUpper(strings.TrimSpace(isrc))
	if isrc == "" {
		return nil, fmt.Errorf("musicbrainz: empty ISRC")
	}
	var body struct {
		Recordings []mbRecording `json:"recordings"`
	}
	err := m.get(ctx, "/isrc/"+url.PathEscape(isrc), url.Values{"inc": {mbRecordingInc}}, &body)
	if err == errNotFound {
		return nil, nil // an unregistered ISRC is an ordinary answer, not a failure
	}
	if err != nil {
		return nil, err
	}
	out := make([]Recording, 0, len(body.Recordings))
	for _, r := range body.Recordings {
		out = append(out, r.toRecording())
	}
	return out, nil
}

// GetRecording implements MusicRecordings.
func (m *MusicBrainz) GetRecording(ctx context.Context, mbid string) (*Recording, error) {
	if strings.TrimSpace(mbid) == "" {
		return nil, fmt.Errorf("musicbrainz: empty recording id")
	}
	var body mbRecording
	q := url.Values{"inc": {mbRecordingInc + "+isrcs"}}
	if err := m.get(ctx, "/recording/"+url.PathEscape(mbid), q, &body); err != nil {
		return nil, err
	}
	r := body.toRecording()
	return &r, nil
}

// GetRelease implements MusicRecordings.
func (m *MusicBrainz) GetRelease(ctx context.Context, mbid string) (*ReleaseDetails, error) {
	if strings.TrimSpace(mbid) == "" {
		return nil, fmt.Errorf("musicbrainz: empty release id")
	}
	var body struct {
		mbRelease
		ArtistCredit []mbArtistCredit `json:"artist-credit"`
		Media        []struct {
			Position int `json:"position"`
			Tracks   []struct {
				Title     string `json:"title"`
				Position  int    `json:"position"`
				Length    int    `json:"length"`
				Recording struct {
					ID             string `json:"id"`
					Disambiguation string `json:"disambiguation"`
					Length         int    `json:"length"`
				} `json:"recording"`
			} `json:"tracks"`
		} `json:"media"`
	}
	q := url.Values{"inc": {"recordings+media+release-groups+artist-credits"}}
	if err := m.get(ctx, "/release/"+url.PathEscape(mbid), q, &body); err != nil {
		return nil, err
	}
	artist, _ := mbJoinCredit(body.ArtistCredit)
	out := &ReleaseDetails{ReleaseRef: body.mbRelease.toRef(), Artist: artist, Tracks: []ReleaseTrack{}}
	for _, md := range body.Media {
		disc := md.Position
		if disc < 1 {
			disc = 1
		}
		for _, t := range md.Tracks {
			length := t.Length
			if length == 0 {
				length = t.Recording.Length
			}
			out.Tracks = append(out.Tracks, ReleaseTrack{
				RecordingMBID: t.Recording.ID, Title: t.Title, Disc: disc, Position: t.Position,
				LengthMs: length, Disambiguation: t.Recording.Disambiguation,
				Versions: mbVersionHints(t.Recording.Disambiguation),
			})
		}
	}
	return out, nil
}

// mbSearchTolerance is how far a recording's length may differ from the wanted duration and
// still be returned: editions of one song routinely differ by a few seconds.
const mbSearchTolerance = 5000 // ms

// luceneQuote wraps s as a Lucene phrase, escaping the characters that would end it early.
func luceneQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// SearchRecordings implements MusicRecordings.
func (m *MusicBrainz) SearchRecordings(ctx context.Context, artist, title string, durationMs int) ([]Recording, error) {
	artist, title = strings.TrimSpace(artist), strings.TrimSpace(title)
	if title == "" {
		return nil, nil
	}
	query := "recording:" + luceneQuote(title)
	if artist != "" {
		query += " AND artist:" + luceneQuote(artist)
	}
	if durationMs > 0 {
		lo := durationMs - mbSearchTolerance
		if lo < 0 {
			lo = 0
		}
		query += " AND dur:[" + strconv.Itoa(lo) + " TO " + strconv.Itoa(durationMs+mbSearchTolerance) + "]"
	}
	var body struct {
		Recordings []mbRecording `json:"recordings"`
	}
	if err := m.get(ctx, "/recording", url.Values{"query": {query}, "limit": {"25"}}, &body); err != nil {
		return nil, err
	}
	out := make([]Recording, 0, len(body.Recordings))
	for _, r := range body.Recordings {
		out = append(out, r.toRecording())
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out, nil
}
