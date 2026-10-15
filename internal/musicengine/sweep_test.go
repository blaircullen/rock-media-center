package musicengine

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type sweepSpec struct {
	Name      string
	Rating    float64 // Plex 0-10; 0 = unrated
	Plays     int
	Playlists int
	NextAt    int64
	NoRec     bool
	NoFile    bool
}

type sweepFix struct {
	t     *testing.T
	v     *env
	dir   string
	ids   map[string]int64
	recs  map[string]int64
	gate  *fakeGate
	sweep *Sweep
}

func newSweepFix(t *testing.T, specs ...sweepSpec) *sweepFix {
	t.Helper()
	f := &sweepFix{t: t, v: newEnv(t), dir: t.TempDir(), ids: map[string]int64{}, recs: map[string]int64{}, gate: &fakeGate{ok: true}}
	for _, s := range specs {
		f.add(s)
	}
	f.sweep = f.newSweep(nil)
	return f
}

func (f *sweepFix) newSweep(resolve func(context.Context, SweepTrack) (int64, error)) *Sweep {
	f.t.Helper()
	s, err := NewSweep(SweepConfig{DB: f.v.db, Space: f.gate, Now: f.v.clock.Now, Local: func(p string) string { return p }, Resolve: resolve})
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *sweepFix) add(s sweepSpec) {
	f.t.Helper()
	path := filepath.Join(f.dir, s.Name+".flac")
	if !s.NoFile {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	var rec any
	if !s.NoRec {
		id := f.v.seedRecording("mbid-"+s.Name, s.Name, "Band", "", "studio")
		f.recs[s.Name] = id
		rec = id
	}
	var rating any
	if s.Rating > 0 {
		rating = s.Rating
	}
	now := f.v.clock.Now().Unix()
	res, err := f.v.db.Exec(
		`INSERT INTO mus_library_tracks (plex_rating_key, path, recording_id, title, artist, album, user_rating, play_count,
		        playlist_count, next_sweep_at, last_seen_at, created_at) VALUES (?, ?, ?, ?, 'Band', 'Album', ?, ?, ?, ?, ?, ?)`,
		"rk-"+s.Name, path, rec, s.Name, rating, s.Plays, s.Playlists, s.NextAt, now, now)
	if err != nil {
		f.t.Fatal(err)
	}
	f.ids[s.Name], _ = res.LastInsertId()
}

func (f *sweepFix) name(trackID int64) string {
	for n, id := range f.ids {
		if id == trackID {
			return n
		}
	}
	return fmt.Sprintf("track-%d", trackID)
}

func (f *sweepFix) run() SweepReport {
	f.t.Helper()
	rep, err := f.sweep.Run(context.Background())
	if err != nil {
		f.t.Fatalf("sweep run: %v", err)
	}
	return rep
}

func (f *sweepFix) finish(itemID int64) {
	f.t.Helper()
	if _, err := f.v.db.Exec(`UPDATE mus_items SET state = 'done' WHERE id = ?`, itemID); err != nil {
		f.t.Fatal(err)
	}
}

func (f *sweepFix) nextInterval(trackID int64) (nextAt, interval int64) {
	f.t.Helper()
	if err := f.v.db.QueryRow(`SELECT next_sweep_at, retry_interval_s FROM mus_library_tracks WHERE id = ?`, trackID).Scan(&nextAt, &interval); err != nil {
		f.t.Fatal(err)
	}
	return nextAt, interval
}

// R20: playlist, then 4 stars or more, then played, then the rest; 1 star is skipped even in a playlist.
func TestSweepVisitsTracksInPlaylistRatedPlayedOtherOrderAndSkipsOneStar(t *testing.T) {
	f := newSweepFix(t,
		sweepSpec{Name: "other-a"},
		sweepSpec{Name: "played", Plays: 5},
		sweepSpec{Name: "four-star", Rating: 8},
		sweepSpec{Name: "playlist", Playlists: 1},
		sweepSpec{Name: "one-star-in-playlist", Rating: 2, Playlists: 3, Plays: 9},
		sweepSpec{Name: "five-star-in-playlist", Rating: 10, Playlists: 2},
		sweepSpec{Name: "three-star", Rating: 6},
		sweepSpec{Name: "half-star", Rating: 1, Plays: 4},
	)
	want := []string{"playlist", "five-star-in-playlist", "four-star", "played", "other-a", "three-star"}
	var got []string
	for i := 0; i < 10; i++ {
		rep := f.run()
		if rep.Created == 0 {
			break
		}
		got = append(got, f.name(rep.Track))
		f.finish(rep.Created)
	}
	// "playlist" and "five-star-in-playlist" are both tier 0; the older row goes first.
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("visit order = %v, want %v", got, want)
	}
	for _, skipped := range []string{"one-star-in-playlist", "half-star"} {
		if n := f.v.count(`SELECT COUNT(*) FROM mus_items WHERE track_id = ?`, f.ids[skipped]); n != 0 {
			t.Errorf("%s was visited although it is rated 1 star or less", skipped)
		}
	}
}

// Smart playlists are not counted: playlist_count comes from non-smart playlists only
// (catalogue.go), so the ordering trusts the column; this pins that the tier follows it.
func TestSweepTierFollowsPlaylistCountNotPlays(t *testing.T) {
	f := newSweepFix(t,
		sweepSpec{Name: "played-a", Plays: 50},
		sweepSpec{Name: "playlist-b", Playlists: 1},
	)
	tracks, err := f.sweep.NextTracks(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 || f.name(tracks[0].ID) != "playlist-b" {
		t.Fatalf("order = %v, want the playlist track first", tracks)
	}
}

// One track at a time through the item table (R23), as a sweep-kind item that starts at searching.
func TestSweepOpensOneItemAtATime(t *testing.T) {
	f := newSweepFix(t, sweepSpec{Name: "a", Playlists: 1}, sweepSpec{Name: "b", Playlists: 1})
	r1 := f.run()
	if r1.Created == 0 {
		t.Fatal("first run opened no item")
	}
	it := f.v.item(r1.Created)
	if it.Kind != KindSweep || it.State != StateSearching || it.TrackID != r1.Track || it.RecordingID != f.recs[f.name(r1.Track)] || it.Priority != PrioritySweep {
		t.Fatalf("item = %+v, want a sweep item in searching for the track and its recording at sweep priority", it)
	}
	if it.DeadlineAt == 0 {
		t.Error("the new item has no deadline for searching")
	}
	r2 := f.run()
	if r2.Created != 0 || r2.Active != 1 {
		t.Fatalf("second run: created %d active %d, want none created while one is active", r2.Created, r2.Active)
	}
	f.finish(r1.Created)
	if r3 := f.run(); r3.Created == 0 || r3.Track == r1.Track {
		t.Fatalf("third run = %+v, want the other track", r3)
	}
}

// An item waiting for Blair holds its recording but does not hold up the next track.
func TestSweepWaitingForBlairDoesNotBlockTheNextTrack(t *testing.T) {
	f := newSweepFix(t, sweepSpec{Name: "a", Playlists: 2}, sweepSpec{Name: "b", Playlists: 1})
	r1 := f.run()
	if _, err := f.v.db.Exec(`UPDATE mus_items SET state = 'waiting_blair' WHERE id = ?`, r1.Created); err != nil {
		t.Fatal(err)
	}
	r2 := f.run()
	if r2.Created == 0 || r2.Track == r1.Track {
		t.Fatalf("second run = %+v, want the next track while the first waits for Blair", r2)
	}
}

// KTD3, R23: a recording that already has an active item (a playlist request) is skipped.
func TestSweepSkipsARecordingWithAnActiveItem(t *testing.T) {
	f := newSweepFix(t, sweepSpec{Name: "busy", Playlists: 3}, sweepSpec{Name: "free", Playlists: 1})
	f.v.seedItem(KindRequest, StateSearching, f.recs["busy"])
	rep := f.run()
	if rep.Created == 0 || f.name(rep.Track) != "free" {
		t.Fatalf("run = %+v, want the track whose recording is free", rep)
	}
	// And if the request item finishes, the busy track is eligible again.
	if _, err := f.v.db.Exec(`UPDATE mus_items SET state = 'done' WHERE kind = 'request'`); err != nil {
		t.Fatal(err)
	}
	f.finish(rep.Created)
	if rep2 := f.run(); rep2.Created == 0 || f.name(rep2.Track) != "busy" {
		t.Fatalf("after the request finished: %+v, want the busy track", rep2)
	}
}

// The index backs the check: a second active item on one recording cannot exist.
func TestSweepItemCannotDuplicateARecordingAtTheDatabaseLevel(t *testing.T) {
	f := newSweepFix(t, sweepSpec{Name: "a", Playlists: 1})
	rep := f.run()
	_, err := f.v.db.Exec(`INSERT INTO mus_items (kind, recording_id, state, entered_at, created_at, updated_at) VALUES ('request', ?, 'new', 1, 1, 1)`, f.recs["a"])
	if err == nil {
		t.Fatalf("a second active item on recording %d was accepted next to sweep item %d", f.recs["a"], rep.Created)
	}
}

// R28: the sweep opens nothing while the space gate says no.
func TestSweepPausedBySpaceGate(t *testing.T) {
	f := newSweepFix(t, sweepSpec{Name: "a", Playlists: 1})
	f.gate.ok, f.gate.reason = false, "free space is under the floor"
	rep := f.run()
	if !rep.Paused || rep.Created != 0 || !strings.Contains(rep.Reason, "floor") {
		t.Fatalf("report = %+v, want paused with the gate's reason", rep)
	}
	if f.v.count(`SELECT COUNT(*) FROM mus_items`) != 0 {
		t.Fatal("an item was opened while paused")
	}
	f.gate.ok = true
	if rep := f.run(); rep.Created == 0 {
		t.Fatal("the sweep did not resume when the gate reopened")
	}
}

// R26: a track not due yet is left alone; a missing file is skipped and rechecked later.
func TestSweepSkipsTracksNotDueAndMissingFiles(t *testing.T) {
	f := newSweepFix(t)
	future := f.v.clock.Now().Add(time.Hour).Unix()
	f.add(sweepSpec{Name: "later", Playlists: 5, NextAt: future})
	f.add(sweepSpec{Name: "gone", Playlists: 4, NoFile: true})
	f.add(sweepSpec{Name: "ok", Playlists: 1})
	rep := f.run()
	if rep.Created == 0 || f.name(rep.Track) != "ok" || rep.Skipped != 1 {
		t.Fatalf("report = %+v, want 'ok' opened and the missing file skipped", rep)
	}
	next, _ := f.nextInterval(f.ids["gone"])
	if want := f.v.clock.Now().Add(sweepRecheckMissing).Unix(); next != want {
		t.Errorf("missing file rechecked at %d, want %d", next, want)
	}
}

// Tracks with no recording id are skipped, or resolved when a resolver is wired.
func TestSweepUnresolvedTracks(t *testing.T) {
	f := newSweepFix(t, sweepSpec{Name: "anon", NoRec: true, Playlists: 1})
	if rep := f.run(); rep.Created != 0 {
		t.Fatalf("an unresolved track was visited without a resolver: %+v", rep)
	}
	rec := f.v.seedRecording("mbid-anon", "anon", "Band", "", "studio")
	f.sweep = f.newSweep(func(_ context.Context, tr SweepTrack) (int64, error) { return rec, nil })
	rep := f.run()
	if rep.Created == 0 || f.v.item(rep.Created).RecordingID != rec {
		t.Fatalf("report = %+v, want an item pinned to the resolved recording", rep)
	}
	// A resolver that fails postpones the track a day and does not stall the run.
	f2 := newSweepFix(t, sweepSpec{Name: "anon", NoRec: true, Playlists: 1}, sweepSpec{Name: "known", Plays: 1})
	f2.sweep = f2.newSweep(func(context.Context, SweepTrack) (int64, error) { return 0, fmt.Errorf("musicbrainz 503") })
	rep2 := f2.run()
	if rep2.Created == 0 || f2.name(rep2.Track) != "known" || rep2.Skipped != 1 {
		t.Fatalf("report = %+v, want the known track after the unresolvable one was skipped", rep2)
	}
}

// R26: each miss doubles the interval from a 7-day base, capped at 90 days.
func TestSweepNextIntervalDoublesAndCaps(t *testing.T) {
	day := 24 * time.Hour
	want := []time.Duration{7 * day, 14 * day, 28 * day, 56 * day, 90 * day, 90 * day}
	var prev time.Duration
	for i, w := range want {
		got := sweepNextInterval(prev, DefaultSweepBase, DefaultSweepMax)
		if got != w {
			t.Fatalf("miss %d: interval %v, want %v", i+1, got, w)
		}
		prev = got
	}
	if got := sweepNextInterval(100*day, DefaultSweepBase, DefaultSweepMax); got != 90*day {
		t.Errorf("an interval over the cap = %v, want 90 days", got)
	}
}

func TestSweepMissTrackSetsTheNextVisitToTheDoubledInterval(t *testing.T) {
	f := newSweepFix(t, sweepSpec{Name: "a", Playlists: 1})
	id := f.ids["a"]
	if _, err := f.v.db.Exec(`UPDATE mus_library_tracks SET retry_interval_s = ? WHERE id = ?`, int64(14*24*3600), id); err != nil {
		t.Fatal(err)
	}
	if err := sweepMissTrack(context.Background(), f.v.db, id, f.v.clock.Now(), 0, 0); err != nil {
		t.Fatal(err)
	}
	next, interval := f.nextInterval(id)
	if interval != 28*24*3600 || next != f.v.clock.Now().Unix()+28*24*3600 {
		t.Errorf("after a miss at 14 days: interval %d next %d, want 28 days", interval, next)
	}
}

// The miss path end to end: a swap step that keeps the incumbent doubles the track's interval
// each time, and the track is not due again until then.
func TestSweepKeepCandidateDoublesTheTrackInterval(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "OLD-GOLD B24", RG: "rg1", Playlists: []string{"P"}})
	var intervals []int64
	for i := 0; i < 3; i++ {
		item, _ := r.seedSwap(inc, swapCandOpts{Audio: fmt.Sprintf("WORSE-%d B16", i)})
		r.mustStep(item)
		if st := r.v.item(item).State; st != StateDropped {
			t.Fatalf("visit %d: item is %s (%q), want dropped", i, st, r.v.item(item).LastReason)
		}
		var iv, next int64
		if err := r.v.db.QueryRow(`SELECT retry_interval_s, next_sweep_at FROM mus_library_tracks WHERE id = ?`, inc.TrackID).Scan(&iv, &next); err != nil {
			t.Fatal(err)
		}
		if next != r.v.clock.Now().Unix()+iv {
			t.Errorf("visit %d: next visit %d, want now + %d s", i, next, iv)
		}
		intervals = append(intervals, iv)
	}
	d := int64(24 * 3600)
	if fmt.Sprint(intervals) != fmt.Sprint([]int64{7 * d, 14 * d, 28 * d}) {
		t.Fatalf("intervals after three misses = %v days, want 7, 14, 28", intervals)
	}
	// Not due: the sweep does not pick the track.
	sw, _ := NewSweep(SweepConfig{DB: r.v.db, Now: r.v.clock.Now, Local: func(p string) string { return p }})
	if tr, err := sw.NextTracks(context.Background(), 10); err != nil || len(tr) != 0 {
		t.Fatalf("NextTracks = %v, %v, want none until the interval passes", tr, err)
	}
	r.v.clock.Advance(29 * 24 * time.Hour)
	if tr, _ := sw.NextTracks(context.Background(), 10); len(tr) != 1 {
		t.Fatalf("NextTracks after the interval = %v, want the track back", tr)
	}
}

// A sweep item the acquirer sent to Blair as "no genuine copy" is a miss, not a question.
func TestSweepSettlesNoGenuineCopyAsAMiss(t *testing.T) {
	f := newSweepFix(t, sweepSpec{Name: "a", Playlists: 1})
	rep := f.run()
	if _, err := f.v.db.Exec(`UPDATE mus_items SET state = 'waiting_blair', deadline_at = 0 WHERE id = ?`, rep.Created); err != nil {
		t.Fatal(err)
	}
	if err := (tableDecisions{}).Raise(context.Background(), f.v.db, DecisionRequest{
		ItemID: rep.Created, Kind: DecisionNoGenuineCopy, Reason: "no genuine lossless copy", Options: []string{"lossy", "wait", "drop"}, Now: f.v.clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// An ordinary request waiting on the same kind of question must be left alone.
	req := f.v.seedItem(KindRequest, StateWaitingBlair, f.v.seedRecording("mbid-req", "req", "Band", "", "studio"))
	if err := (tableDecisions{}).Raise(context.Background(), f.v.db, DecisionRequest{ItemID: req, Kind: DecisionNoGenuineCopy, Reason: "x", Options: []string{"drop"}, Now: f.v.clock.Now()}); err != nil {
		t.Fatal(err)
	}

	rep2 := f.run()
	if rep2.SettledMisses != 1 {
		t.Fatalf("settled misses = %d, want 1", rep2.SettledMisses)
	}
	if st := f.v.item(rep.Created).State; st != StateDropped {
		t.Errorf("sweep item is %s, want dropped", st)
	}
	if f.v.openDecisions(rep.Created) != 0 {
		t.Error("the decision stayed open")
	}
	if st := f.v.item(req).State; st != StateWaitingBlair || f.v.openDecisions(req) != 1 {
		t.Errorf("the request item was touched: %s, %d open decisions", st, f.v.openDecisions(req))
	}
	if _, iv := f.nextInterval(f.ids["a"]); iv != int64(DefaultSweepBase/time.Second) {
		t.Errorf("retry interval = %d, want the 7-day base", iv)
	}
}

func TestSweepSettleKeepsIsIdempotent(t *testing.T) {
	f := newSweepFix(t, sweepSpec{Name: "a", Playlists: 1})
	rep := f.run()
	var cand int64
	res, err := f.v.db.Exec(`INSERT INTO mus_candidates (item_id, lane, state, pcm_fingerprint, created_at) VALUES (?, 'usenet', 'chosen', 'fp-1', 1)`, rep.Created)
	if err != nil {
		t.Fatal(err)
	}
	cand, _ = res.LastInsertId()
	if err := (tableDecisions{}).Raise(context.Background(), f.v.db, DecisionRequest{ItemID: rep.Created, Kind: DecisionTradeOff, Reason: "x", Options: []string{"approve", "keep"}, Now: f.v.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.v.db.Exec(`UPDATE mus_decisions SET candidate_id = ?, answer = 'keep', answered_at = 5 WHERE item_id = ?`, cand, rep.Created); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		f.run()
	}
	var n int
	var rec sql.NullInt64
	if err := f.v.db.QueryRow(`SELECT COUNT(*), MIN(recording_id) FROM mus_blocked_candidates WHERE pcm_fingerprint = 'fp-1'`).Scan(&n, &rec); err != nil {
		t.Fatal(err)
	}
	if n != 1 || rec.Int64 != f.recs["a"] {
		t.Errorf("blocked rows = %d (recording %d), want one for the track's recording", n, rec.Int64)
	}
}

func TestNewSweepNeedsADatabase(t *testing.T) {
	if _, err := NewSweep(SweepConfig{}); err == nil {
		t.Fatal("NewSweep accepted a config with no database")
	}
}
