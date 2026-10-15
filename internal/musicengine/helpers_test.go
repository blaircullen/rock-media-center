package musicengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tristenlammi/arrmada/internal/audiocheck"
	"github.com/tristenlammi/arrmada/internal/metadata"
	"github.com/tristenlammi/arrmada/internal/plex"
	"github.com/tristenlammi/arrmada/internal/spotify"
	"github.com/tristenlammi/arrmada/internal/store"
)

// ---- clock ----

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// ---- fakes ----

type fakeSpotify struct {
	mu    sync.Mutex
	pl    *spotify.Playlist
	err   error
	calls int
}

func (f *fakeSpotify) ReadPlaylist(_ context.Context, _ string) (*spotify.Playlist, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	cp := *f.pl
	return &cp, nil
}

func (f *fakeSpotify) set(entries ...spotify.Entry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = nil
	f.pl = &spotify.Playlist{
		PlaylistInfo: spotify.PlaylistInfo{ID: "pl1", Name: "My Shazam Tracks", OwnerID: "sunygxc", SnapshotID: "snap", Total: len(entries)},
		Entries:      entries,
	}
}

type fakeMB struct {
	mu      sync.Mutex
	byISRC  map[string][]metadata.Recording
	byMBID  map[string]*metadata.Recording
	search  []metadata.Recording
	err     error
	classes []metadata.MBClass
	gets    map[string]int
}

func newFakeMB() *fakeMB {
	return &fakeMB{byISRC: map[string][]metadata.Recording{}, byMBID: map[string]*metadata.Recording{}, gets: map[string]int{}}
}

func (f *fakeMB) add(r metadata.Recording) {
	cp := r
	f.byMBID[r.MBID] = &cp
	for _, i := range r.ISRCs {
		f.byISRC[i] = append(f.byISRC[i], r)
	}
}

func (f *fakeMB) note(ctx context.Context) {
	f.classes = append(f.classes, metadata.MBClassOf(ctx))
}

func (f *fakeMB) RecordingsByISRC(ctx context.Context, isrc string) ([]metadata.Recording, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note(ctx)
	if f.err != nil {
		return nil, f.err
	}
	return f.byISRC[isrc], nil
}

func (f *fakeMB) GetRecording(ctx context.Context, mbid string) (*metadata.Recording, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note(ctx)
	f.gets[mbid]++
	if f.err != nil {
		return nil, f.err
	}
	r, ok := f.byMBID[mbid]
	if !ok {
		return nil, fmt.Errorf("musicbrainz: %s not found", mbid)
	}
	cp := *r
	return &cp, nil
}

func (f *fakeMB) GetRelease(context.Context, string) (*metadata.ReleaseDetails, error) {
	return nil, errors.New("not used")
}

func (f *fakeMB) SearchRecordings(ctx context.Context, _, _ string, _ int) ([]metadata.Recording, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note(ctx)
	if f.err != nil {
		return nil, f.err
	}
	return f.search, nil
}

type fakePlex struct {
	mu          sync.Mutex
	tracks      []plex.Track
	search      map[string][]plex.Track // key "artist|title", lower-cased
	playlists   []plex.Playlist
	items       map[string][]plex.PlaylistItem
	searchCalls int
	searchErr   error
	playlistErr error
}

func newFakePlex() *fakePlex {
	return &fakePlex{search: map[string][]plex.Track{}, items: map[string][]plex.PlaylistItem{}}
}

func (f *fakePlex) Tracks(context.Context, string) ([]plex.Track, error) { return f.tracks, nil }

func (f *fakePlex) SearchTracks(_ context.Context, _, artist, title string) ([]plex.Track, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searchCalls++
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return f.search[strings.ToLower(artist+"|"+title)], nil
}

func (f *fakePlex) Playlists(context.Context) ([]plex.Playlist, error) { return f.playlists, nil }

func (f *fakePlex) PlaylistItems(_ context.Context, key string) ([]plex.PlaylistItem, error) {
	if f.playlistErr != nil {
		return nil, f.playlistErr
	}
	return f.items[key], nil
}

type fakeChecker struct {
	mu      sync.Mutex
	results map[string][]audiocheck.Result // consumed in order, the last one repeats
	calls   map[string]int
}

func newFakeChecker() *fakeChecker {
	return &fakeChecker{results: map[string][]audiocheck.Result{}, calls: map[string]int{}}
}

func (f *fakeChecker) Check(_ context.Context, path string) audiocheck.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[path]++
	rs := f.results[path]
	if len(rs) == 0 {
		return audiocheck.Result{Path: path, Verdict: audiocheck.Blind, Reason: "no fake result for " + path}
	}
	i := min(f.calls[path]-1, len(rs)-1)
	r := rs[i]
	r.Path = path
	return r
}

func genuineFLAC() audiocheck.Result {
	return audiocheck.Result{Verdict: audiocheck.Genuine, Reason: "ok", Facts: audiocheck.Facts{Codec: "flac", BitDepth: 16, SampleRate: 44100, Channels: 2, BitrateKbps: 900, DurationSec: 200}}
}

func verdictResult(v audiocheck.Verdict, codec string) audiocheck.Result {
	r := genuineFLAC()
	r.Verdict, r.Reason, r.Facts.Codec = v, "test", codec
	return r
}

type fakeTags struct {
	tags map[string]Tags
	errs map[string]error
}

func newFakeTags() *fakeTags { return &fakeTags{tags: map[string]Tags{}, errs: map[string]error{}} }

func (f *fakeTags) ReadTags(_ context.Context, path string) (Tags, error) {
	if err := f.errs[path]; err != nil {
		return Tags{}, err
	}
	t, ok := f.tags[path]
	if !ok {
		return Tags{}, fmt.Errorf("no fake tags for %s", path)
	}
	return t, nil
}

type recNotifier struct {
	mu     sync.Mutex
	alerts []Alert
	err    error
}

func (n *recNotifier) Alert(_ context.Context, a Alert) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.err != nil {
		return n.err
	}
	n.alerts = append(n.alerts, a)
	return nil
}

func (n *recNotifier) count(kind AlertKind) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := 0
	for _, a := range n.alerts {
		if a.Kind == kind {
			c++
		}
	}
	return c
}

type countingStep struct {
	mu    sync.Mutex
	calls []int64
}

func (c *countingStep) Step(_ context.Context, _ Machine, it Item) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, it.ID)
	return nil
}

type fakeGate struct {
	ok     bool
	reason string
}

func (g *fakeGate) Allow(context.Context, WorkKind) (bool, string) { return g.ok, g.reason }

// ---- environment ----

type env struct {
	t        *testing.T
	st       *store.Store
	db       *sql.DB
	e        *Engine
	clock    *fakeClock
	spotify  *fakeSpotify
	mb       *fakeMB
	plex     *fakePlex
	checker  *fakeChecker
	tags     *fakeTags
	notifier *recNotifier
	acquirer *countingStep
	settings Settings
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	v := &env{
		t: t, st: st, db: st.DB(),
		clock:    &fakeClock{t: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)},
		spotify:  &fakeSpotify{},
		mb:       newFakeMB(),
		plex:     newFakePlex(),
		checker:  newFakeChecker(),
		tags:     newFakeTags(),
		notifier: &recNotifier{},
		acquirer: &countingStep{},
		settings: Settings{PlaylistID: "pl1", PlexSection: "5"},
	}
	v.spotify.set()
	v.e = v.newEngine()
	return v
}

// newEngine builds a fresh Engine over the same database, as a restart would.
func (v *env) newEngine() *Engine {
	v.t.Helper()
	e, err := New(Config{
		DB: v.db, Settings: func() Settings { return v.settings },
		Spotify: v.spotify, MB: v.mb, Plex: v.plex, Checker: v.checker, Tags: v.tags,
		Acquirer: v.acquirer, Notifier: v.notifier,
		Now: v.clock.Now,
	})
	if err != nil {
		v.t.Fatalf("new engine: %v", err)
	}
	return e
}

func entry(id, name, artist, isrc string) spotify.Entry {
	return spotify.Entry{Type: "track", TrackID: id, ISRC: isrc, Name: name, Artists: []string{artist}, Album: name + " album", DurationMs: 200000}
}

func (v *env) sync() SyncReport {
	v.t.Helper()
	rep, err := v.e.SyncIntake(context.Background())
	if err != nil {
		v.t.Fatalf("SyncIntake: %v", err)
	}
	return rep
}

func (v *env) itemFor(trackID string) Item {
	v.t.Helper()
	var id int64
	err := v.db.QueryRow(`SELECT item_id FROM mus_playlist_entries WHERE spotify_track_id = ?`, trackID).Scan(&id)
	if err != nil {
		v.t.Fatalf("item for %s: %v", trackID, err)
	}
	it, err := v.e.GetItem(context.Background(), id)
	if err != nil {
		v.t.Fatal(err)
	}
	return it
}

func (v *env) item(id int64) Item {
	v.t.Helper()
	it, err := v.e.GetItem(context.Background(), id)
	if err != nil {
		v.t.Fatal(err)
	}
	return it
}

func (v *env) tick(n int) {
	v.t.Helper()
	for i := 0; i < n; i++ {
		if err := v.e.Tick(context.Background()); err != nil {
			v.t.Logf("tick %d: %v", i, err)
		}
	}
}

func (v *env) count(query string, args ...any) int {
	v.t.Helper()
	var n int
	if err := v.db.QueryRow(query, args...).Scan(&n); err != nil {
		v.t.Fatalf("%s: %v", query, err)
	}
	return n
}

func (v *env) openDecisions(itemID int64) int {
	return v.count(`SELECT COUNT(*) FROM mus_decisions WHERE item_id = ? AND answered_at = 0`, itemID)
}

// seedItem inserts an item directly in a given state (and optional recording), for state
// machine tests that do not need intake.
func (v *env) seedItem(kind string, state State, recordingID int64) int64 {
	v.t.Helper()
	now := v.clock.Now()
	var rec any
	if recordingID != 0 {
		rec = recordingID
	}
	res, err := v.db.Exec(
		`INSERT INTO mus_items (kind, recording_id, state, entered_at, deadline_at, priority, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		kind, rec, string(state), now.Unix(), DeadlineFor(state, now), PrioritySweep, now.Unix(), now.Unix())
	if err != nil {
		v.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (v *env) seedRecording(mbid, title, artist, isrc, hint string) int64 {
	v.t.Helper()
	id, err := upsertRecording(context.Background(), v.db, Recording{
		MBID: mbid, Title: title, Artist: artist, ISRC: isrc, LengthMs: 200000, VersionHint: hint,
	}, v.clock.Now())
	if err != nil {
		v.t.Fatal(err)
	}
	return id
}

// studio and variants of one song, as MusicBrainz would describe them.
func mbRec(mbid, title, artist, disambig, isrc string) metadata.Recording {
	r := metadata.Recording{
		MBID: mbid, Title: title, Artist: artist, LengthMs: 200000, Disambiguation: disambig,
		Versions: versionHints(disambig),
		Releases: []metadata.ReleaseRef{{MBID: "rel-" + mbid, Title: title + " album", Status: "Official", Date: "2010-01-01", ReleaseGroupMBID: "rg-" + mbid}},
	}
	if isrc != "" {
		r.ISRCs = []string{isrc}
	}
	return r
}
