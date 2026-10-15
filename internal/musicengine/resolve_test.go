package musicengine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tristenlammi/arrmada/internal/metadata"
)

type resolveFix struct {
	t  *testing.T
	v  *env
	r  *Resolver
	id int64 // the library track
}

func newResolveFix(t *testing.T) *resolveFix {
	t.Helper()
	v := newEnv(t)
	r, err := NewResolver(ResolverConfig{DB: v.db, Tags: v.tags, MB: v.mb, Now: v.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	now := v.clock.Now().Unix()
	res, err := v.db.Exec(
		`INSERT INTO mus_library_tracks (plex_rating_key, path, title, artist, album, last_seen_at, created_at)
		 VALUES ('rk1', '/music/Band/Song.flac', 'Song', 'Band', 'Song album', ?, ?)`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return &resolveFix{t: t, v: v, r: r, id: id}
}

func (f *resolveFix) resolve() (int64, error) {
	return f.r.Resolve(context.Background(), SweepTrack{ID: f.id, PlexPath: "/music/Band/Song.flac", Artist: "Band", Title: "Song", Album: "Song album"})
}

func (f *resolveFix) stored() (rec int64, ok bool) {
	var n *int64
	if err := f.v.db.QueryRow(`SELECT recording_id FROM mus_library_tracks WHERE id = ?`, f.id).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	if n == nil {
		return 0, false
	}
	return *n, true
}

func (f *resolveFix) mbid(id int64) string {
	var m string
	if err := f.v.db.QueryRow(`SELECT mbid FROM mus_recordings WHERE id = ?`, id).Scan(&m); err != nil {
		f.t.Fatal(err)
	}
	return m
}

// A recording id in the tags is fetched once, stored, written to the track, and asked for
// in the sweep class.
func TestResolveFromTagRecordingID(t *testing.T) {
	f := newResolveFix(t)
	f.v.mb.add(mbRec("rec-1", "Song", "Band", "", ""))
	f.v.tags.tags["/music/Band/Song.flac"] = Tags{RecordingMBID: "rec-1", Title: "Song", Artist: "Band"}

	id, err := f.resolve()
	if err != nil || id == 0 {
		t.Fatalf("Resolve = %d, %v", id, err)
	}
	if got, ok := f.stored(); !ok || got != id || f.mbid(id) != "rec-1" {
		t.Errorf("track recording = %d/%v, want %d for rec-1", got, ok, id)
	}
	if len(f.v.mb.classes) != 1 || f.v.mb.classes[0] != metadata.MBSweep {
		t.Errorf("MusicBrainz classes = %v, want one sweep request", f.v.mb.classes)
	}
}

// A recording already stored needs no MusicBrainz request.
func TestResolveKnownRecordingMakesNoRequest(t *testing.T) {
	f := newResolveFix(t)
	want := f.v.seedRecording("rec-known", "Song", "Band", "", "studio")
	f.v.tags.tags["/music/Band/Song.flac"] = Tags{RecordingMBID: "rec-known"}

	id, err := f.resolve()
	if err != nil || id != want {
		t.Fatalf("Resolve = %d, %v, want %d", id, err, want)
	}
	if len(f.v.mb.classes) != 0 {
		t.Errorf("MusicBrainz was asked %d times, want none", len(f.v.mb.classes))
	}
	if got, ok := f.stored(); !ok || got != want {
		t.Errorf("track recording = %d/%v", got, ok)
	}
}

func TestResolveByISRCUnambiguous(t *testing.T) {
	f := newResolveFix(t)
	f.v.mb.add(mbRec("rec-isrc", "Song", "Band", "", "USABC1234567"))
	f.v.tags.tags["/music/Band/Song.flac"] = Tags{ISRC: "usabc1234567", Title: "Song", Artist: "Band", DurationSec: 200}

	id, err := f.resolve()
	if err != nil || f.mbid(id) != "rec-isrc" {
		t.Fatalf("Resolve = %d, %v", id, err)
	}
}

// Two recordings under one ISRC that title and length cannot separate is a skip, and the
// track is left unresolved.
func TestResolveByISRCAmbiguousSkips(t *testing.T) {
	f := newResolveFix(t)
	f.v.mb.add(mbRec("rec-a", "Song", "Band", "", "USABC1234567"))
	f.v.mb.add(mbRec("rec-b", "Song", "Band", "", "USABC1234567"))
	f.v.tags.tags["/music/Band/Song.flac"] = Tags{ISRC: "USABC1234567", Title: "Song", Artist: "Band", DurationSec: 200}

	id, err := f.resolve()
	if id != 0 || !errors.Is(err, ErrResolveSkip) {
		t.Fatalf("Resolve = %d, %v, want a skip", id, err)
	}
	if _, ok := f.stored(); ok {
		t.Error("an ambiguous ISRC wrote a recording to the track")
	}
}

func TestResolveBySearchUnique(t *testing.T) {
	f := newResolveFix(t)
	f.v.mb.search = []metadata.Recording{
		mbRec("rec-s", "Song", "Band", "", ""),
		mbRec("rec-other", "Other Song", "Band", "", ""),
		mbRec("rec-live", "Song", "Band", "live", ""),
	}
	f.v.tags.tags["/music/Band/Song.flac"] = Tags{Title: "Song", Artist: "Band", Album: "Song album", DurationSec: 200}

	id, err := f.resolve()
	if err != nil || f.mbid(id) != "rec-s" {
		t.Fatalf("Resolve = %d, %v (want only the studio Song)", id, err)
	}
}

// Two equally good search results are told apart by the track's album.
func TestResolveBySearchAlbumBreaksTie(t *testing.T) {
	f := newResolveFix(t)
	a := mbRec("rec-a", "Song", "Band", "", "")
	a.Releases = []metadata.ReleaseRef{{MBID: "r1", Title: "Greatest Hits"}}
	b := mbRec("rec-b", "Song", "Band", "", "")
	b.Releases = []metadata.ReleaseRef{{MBID: "r2", Title: "Song album"}}
	f.v.mb.search = []metadata.Recording{a, b}
	f.v.tags.tags["/music/Band/Song.flac"] = Tags{Title: "Song", Artist: "Band", Album: "Song album", DurationSec: 200}

	id, err := f.resolve()
	if err != nil || f.mbid(id) != "rec-b" {
		t.Fatalf("Resolve = %d, %v, want rec-b", id, err)
	}
}

func TestResolveBySearchAmbiguousSkips(t *testing.T) {
	f := newResolveFix(t)
	f.v.mb.search = []metadata.Recording{mbRec("rec-a", "Song", "Band", "", ""), mbRec("rec-b", "Song", "Band", "", "")}
	f.v.tags.tags["/music/Band/Song.flac"] = Tags{Title: "Song", Artist: "Band", Album: "Unrelated", DurationSec: 200}

	if id, err := f.resolve(); id != 0 || !errors.Is(err, ErrResolveSkip) {
		t.Fatalf("Resolve = %d, %v, want a skip", id, err)
	}
	if _, ok := f.stored(); ok {
		t.Error("an ambiguous search wrote a recording to the track")
	}
}

// A search result of the wrong length is not the song.
func TestResolveBySearchRejectsWrongLength(t *testing.T) {
	f := newResolveFix(t)
	long := mbRec("rec-long", "Song", "Band", "", "")
	long.LengthMs = 320000
	f.v.mb.search = []metadata.Recording{long}
	f.v.tags.tags["/music/Band/Song.flac"] = Tags{Title: "Song", Artist: "Band", DurationSec: 200}

	if id, err := f.resolve(); id != 0 || !errors.Is(err, ErrResolveSkip) {
		t.Fatalf("Resolve = %d, %v, want a skip", id, err)
	}
}

// An unreadable file and a MusicBrainz failure are both skips that keep the cause.
func TestResolveKeepsTheCauseOfAFailure(t *testing.T) {
	f := newResolveFix(t)
	boom := errors.New("ffprobe: i/o error")
	f.v.tags.errs["/music/Band/Song.flac"] = boom
	if id, err := f.resolve(); id != 0 || !errors.Is(err, ErrResolveSkip) || !errors.Is(err, boom) {
		t.Fatalf("tag failure: Resolve = %d, %v", id, err)
	}

	f.v.tags.errs = map[string]error{}
	f.v.tags.tags["/music/Band/Song.flac"] = Tags{Title: "Song", Artist: "Band"}
	mbDown := errors.New("musicbrainz: 503")
	f.v.mb.err = mbDown
	if id, err := f.resolve(); id != 0 || !errors.Is(err, ErrResolveSkip) || !errors.Is(err, mbDown) {
		t.Fatalf("musicbrainz failure: Resolve = %d, %v", id, err)
	}
}

// Wired as the sweep's resolver, an unresolved track becomes a sweep item.
func TestResolveFeedsTheSweep(t *testing.T) {
	f := newResolveFix(t)
	path := filepath.Join(t.TempDir(), "Song.flac")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.v.db.Exec(`UPDATE mus_library_tracks SET path = ? WHERE id = ?`, path, f.id); err != nil {
		t.Fatal(err)
	}
	f.v.mb.add(mbRec("rec-1", "Song", "Band", "", ""))
	f.v.tags.tags[path] = Tags{RecordingMBID: "rec-1"}
	s, err := NewSweep(SweepConfig{DB: f.v.db, Now: f.v.clock.Now, Local: func(p string) string { return p }, Resolve: f.r.Resolve})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Created == 0 || rep.Track != f.id {
		t.Fatalf("sweep report = %+v, want an item for track %d", rep, f.id)
	}
	if _, ok := f.stored(); !ok {
		t.Error("the sweep ran without the resolver writing recording_id")
	}
}
