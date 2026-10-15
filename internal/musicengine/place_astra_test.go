package musicengine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tristenlammi/arrmada/internal/metadata"
)

// Tests for the Astra revision pass over placement, swaps, undo and recovery
// (findings 1, 2, 6, 13 and 15).

// ---- finding 1: one writer per destination, never overwrite quarantine ----

func TestQuarantineCopyNeverOverwritesAnExistingFile(t *testing.T) {
	s := newSwapRig(t)
	row, err := s.pl.Stage(context.Background(), s.spec())
	if err != nil {
		t.Fatal(err)
	}
	// A different file already sits at the quarantine destination (a saved version).
	if err := os.MkdirAll(filepath.Dir(s.quarPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.quarPath, []byte("A SAVED VERSION"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pl.Advance(context.Background(), row); err == nil {
		t.Fatal("Advance succeeded over a quarantine file holding other bytes")
	}
	if b, _ := os.ReadFile(s.quarPath); string(b) != "A SAVED VERSION" {
		t.Fatalf("the existing quarantine file was overwritten: %q", b)
	}
	if got, _ := placeSHA256(s.dest); got != s.oldSHA {
		t.Error("the library file changed although the quarantine copy could not be made")
	}
}

func TestQuarantineCopyReusesAnIdenticalExistingFile(t *testing.T) {
	s := newSwapRig(t)
	row, err := s.pl.Stage(context.Background(), s.spec())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(s.quarPath), 0o755); err != nil {
		t.Fatal(err)
	}
	old, _ := os.ReadFile(s.dest)
	if err := os.WriteFile(s.quarPath, old, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := s.pl.Advance(context.Background(), row); err != nil || out != OutcomeAdvanced {
		t.Fatalf("Advance = %v %v, want advanced", out, err)
	}
	if got, _ := placeSHA256(s.dest); got != s.newSHA {
		t.Error("the swap did not publish")
	}
	for name := range placeTree(t, s.quar) {
		if filepath.Ext(name) != ".flac" {
			t.Errorf("stray file left in quarantine: %s", name)
		}
	}
}

// A second Advance holding a copy of the row from before the first one ran must act on the
// journal's current state, not its own stale copy.
func TestAdvanceReadsTheRowAfterTakingOwnership(t *testing.T) {
	s := newSwapRig(t)
	row, err := s.pl.Stage(context.Background(), s.spec())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pl.Advance(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	decisions := s.v.count(`SELECT COUNT(*) FROM mus_decisions`)
	out, err := s.pl.Advance(context.Background(), row) // the stale copy: phase temp_verified
	if err != nil || out != OutcomeAdvanced {
		t.Fatalf("stale Advance = %v %v, want a quiet advanced", out, err)
	}
	if n := s.v.count(`SELECT COUNT(*) FROM mus_decisions`); n != decisions {
		t.Errorf("the stale Advance raised %d decisions", n-decisions)
	}
	if got, _ := placeSHA256(s.dest); got != s.newSHA {
		t.Error("the library file is not the swapped-in version")
	}
	if q, _ := placeSHA256(s.quarPath); q != s.oldSHA {
		t.Error("the saved version is not intact in quarantine")
	}
}

func TestConcurrentAdvanceOfOneRowPublishesOnce(t *testing.T) {
	s := newSwapRig(t)
	row, err := s.pl.Stage(context.Background(), s.spec())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.pl.Advance(context.Background(), row); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Advance: %v", err)
	}
	if got, _ := placeSHA256(s.dest); got != s.newSHA {
		t.Error("the swap did not publish")
	}
	if q, _ := placeSHA256(s.quarPath); q != s.oldSHA {
		t.Error("the saved version is not intact in quarantine")
	}
	if n := s.v.count(`SELECT COUNT(*) FROM mus_quarantine WHERE path = ?`, s.quarPath); n != 1 {
		t.Errorf("quarantine rows = %d, want 1", n)
	}
	if left := placeTemps(t, s.lib); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

// ---- finding 2: the codec gate at final publication ----

func TestPlacementRefusesALossyCodecUnlessItIsTheApprovedYouTubeCase(t *testing.T) {
	r := newPlaceRig(t)
	s := r.seed(placeSeed{Title: "Song", Artist: "Band", Album: "Album", Disc: 1, Track: 1, Audio: "AACCODEC-ONE"})
	if err := r.step(s.ItemID); err != nil {
		t.Fatal(err)
	}
	if it := r.v.item(s.ItemID); it.State != StateSearching {
		t.Fatalf("item = %s, want searching after the lossy file was refused", it.State)
	}
	if n := r.v.count(`SELECT COUNT(*) FROM mus_candidates WHERE id = ? AND state = 'rejected'`, s.CandID); n != 1 {
		t.Error("the lossy candidate was not rejected")
	}
	if got := r.libFlacs(); len(got) != 0 {
		t.Fatalf("a lossy file reached the library: %v", got)
	}
	if left := placeTemps(t, r.lib); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}

	// Plan R15: the official YouTube audio Blair approved is stored as a youtube-lane
	// candidate with the verdict LOSSY, and may be placed.
	y := r.seed(placeSeed{Title: "Other", Artist: "Band", Album: "Album", Disc: 1, Track: 2, Audio: "AACCODEC-TWO"})
	if _, err := r.v.db.Exec(`UPDATE mus_candidates SET lane = ?, verdict = 'LOSSY' WHERE id = ?`, LaneYouTube, y.CandID); err != nil {
		t.Fatal(err)
	}
	r.drive(y.ItemID, 4)
	if got := r.libFlacs(); len(got) != 1 {
		t.Fatalf("library = %v, want the approved YouTube file", got)
	}
}

func TestUndoMayRestoreALossyFile(t *testing.T) {
	s := newSwapRig(t)
	lossy := filepath.Join(s.stage, "old.flac")
	sha := placeWrite(t, lossy, "AACCODEC-OLD", TrackTags{Artist: "Band", Album: "Album", Title: "Song", Disc: 1, Track: 1})
	row, err := s.pl.Stage(context.Background(), StageSpec{
		Kind: JournalUndo, Source: lossy, SourceSHA: sha, Dest: s.dest, IncumbentSHA: s.oldSHA,
		QuarantinePath: s.quarPath, ExpectFingerprint: placeFP("AACCODEC-OLD"),
	})
	if err != nil {
		t.Fatalf("undo of a lossy original was refused: %v", err)
	}
	if row.Phase != PhaseTempVerified {
		t.Errorf("phase = %d, want temp_verified", row.Phase)
	}
}

// ---- finding 6: space reservations ----

type placeSpy struct {
	mu       sync.Mutex
	refuse   bool
	reserved map[int64]int64
	calls    []int64
	released []int64
}

func (p *placeSpy) Reserve(_ context.Context, key, bytes int64) (bool, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, key)
	if p.refuse {
		return false, "the library is below its floor"
	}
	if p.reserved == nil {
		p.reserved = map[int64]int64{}
	}
	p.reserved[key] = bytes
	return true, ""
}

func (p *placeSpy) Release(_ context.Context, key int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.released = append(p.released, key)
	delete(p.reserved, key)
}

func TestPlacementWaitsForSpaceInsteadOfCopying(t *testing.T) {
	r := newPlaceRig(t)
	spy := &placeSpy{refuse: true}
	r.pl.cfg.Space = spy
	s := r.seed(placeSeed{Title: "Song", Artist: "Band", Album: "Album", Disc: 1, Track: 1})

	if err := r.step(s.ItemID); err != nil {
		t.Fatalf("a space refusal must be a wait, not an error: %v", err)
	}
	if it := r.v.item(s.ItemID); it.State != StatePlacing || it.NextAttemptAt == 0 {
		t.Fatalf("item = %s next_attempt_at %d, want placing and scheduled for later", it.State, it.NextAttemptAt)
	}
	if n := r.v.count(`SELECT COUNT(*) FROM mus_swap_journal`); n != 0 {
		t.Errorf("a journal row was started (%d) although there was no room", n)
	}
	if n := r.v.count(`SELECT COUNT(*) FROM mus_candidates WHERE id = ? AND state = 'chosen'`, s.CandID); n != 1 {
		t.Error("the candidate was dropped by the wait")
	}
	if left := placeTemps(t, r.lib); len(left) != 0 {
		t.Errorf("temp files: %v", left)
	}

	spy.refuse = false
	r.drive(s.ItemID, 4)
	for _, k := range spy.calls {
		if k >= 0 {
			t.Errorf("reservation key %d is not negative; it could collide with an item id", k)
		}
	}
	if len(spy.reserved) != 0 {
		t.Errorf("reservations still held after the journal completed: %v", spy.reserved)
	}
}

func TestSwapReservesTheQuarantineCopyToo(t *testing.T) {
	s := newSwapRig(t)
	spy := &placeSpy{}
	s.pl.cfg.Space = spy
	if err := s.run(); err != nil {
		t.Fatal(err)
	}
	if len(spy.calls) < 2 {
		t.Fatalf("reservations = %d, want one for the temp copy and one for the quarantine copy", len(spy.calls))
	}
	if len(spy.reserved) != 0 {
		t.Errorf("reservations still held after publishing: %v", spy.reserved)
	}
}

func TestAdvanceLeavesTheRowWaitingWhenQuarantineHasNoRoom(t *testing.T) {
	s := newSwapRig(t)
	row, err := s.pl.Stage(context.Background(), s.spec())
	if err != nil {
		t.Fatal(err)
	}
	spy := &placeSpy{refuse: true}
	s.pl.cfg.Space = spy
	if _, err := s.pl.Advance(context.Background(), row); !errors.Is(err, ErrSpaceWait) {
		t.Fatalf("Advance err = %v, want ErrSpaceWait", err)
	}
	if _, err := os.Stat(s.quarPath); err == nil {
		t.Error("the quarantine copy was made without room")
	}
	if cur, _ := s.pl.jr.Get(context.Background(), row.ID); !cur.Open() || cur.Phase != PhaseTempVerified {
		t.Errorf("row open=%v phase=%d, want open at temp_verified", cur.Open(), cur.Phase)
	}
	spy.refuse = false
	if _, err := s.pl.Advance(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if got, _ := placeSHA256(s.dest); got != s.newSHA {
		t.Error("the swap did not publish once there was room")
	}
}

// ---- finding 13: the playing check sits right before publication ----

func TestSwapPublicationWaitsWhileThePlayingTrackPlays(t *testing.T) {
	s := newSwapRig(t)
	playing := true
	s.pl.playing = func(context.Context, JournalRow) (bool, error) { return playing, nil }
	row, err := s.pl.Stage(context.Background(), s.spec())
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.pl.Advance(context.Background(), row)
	if err != nil || out != OutcomePending {
		t.Fatalf("Advance = %v %v, want pending", out, err)
	}
	if got, _ := placeSHA256(s.dest); got != s.oldSHA {
		t.Fatal("the library file changed under playback")
	}
	if cur, _ := s.pl.jr.Get(context.Background(), row.ID); !cur.Open() {
		t.Fatal("the row was closed; it must stay open for the next pass")
	}

	// Boot reconciliation takes the same path and must not publish either.
	s.restart()
	s.pl.playing = func(context.Context, JournalRow) (bool, error) { return playing, nil }
	rep := s.mustReconcile()
	if rep.Pending != 1 {
		t.Errorf("reconcile report %+v, want one pending row", rep)
	}
	if got, _ := placeSHA256(s.dest); got != s.oldSHA {
		t.Fatal("reconcile replaced a track that is playing")
	}

	playing = false
	rep = s.mustReconcile()
	if got, _ := placeSHA256(s.dest); got != s.newSHA || rep.Advanced != 1 {
		t.Errorf("after playback stopped: dest is new=%v, report %+v", got == s.newSHA, rep)
	}
}

func TestPlayingCheckErrorKeepsTheLibraryFileUntouched(t *testing.T) {
	s := newSwapRig(t)
	s.pl.playing = func(context.Context, JournalRow) (bool, error) { return false, errors.New("Plex is down") }
	row, err := s.pl.Stage(context.Background(), s.spec())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pl.Advance(context.Background(), row); err == nil {
		t.Fatal("Advance went ahead although Plex could not say whether the track is playing")
	}
	if got, _ := placeSHA256(s.dest); got != s.oldSHA {
		t.Error("the library file changed")
	}
}

func TestUndoRecoveryDoesNotReplaceAPlayingTrack(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "A-OLD SUSPECT", RG: "rg1", Rating: 6})
	swap1 := r.swapTo(inc, "B-NEW B24")
	// Sessions is asked once by Undo's own check and once right before the rename; the track
	// starts playing in between.
	r.fp.playFrom, r.fp.playFromKey = r.fp.sessCalls+2, r.keyOf(inc.TrackID)

	res, err := r.sw.Undo(context.Background(), swap1)
	if !errors.Is(err, ErrPlaybackPending) {
		t.Fatalf("Undo err = %v (%+v), want ErrPlaybackPending", err, res)
	}
	if r.audioOf(inc.Local) != "B-NEW B24" {
		t.Fatal("the undo replaced a playing track")
	}
	if n := r.v.count(`SELECT COUNT(*) FROM mus_swap_journal WHERE kind = 'undo' AND closed_at = 0`); n != 1 {
		t.Fatalf("open undo rows = %d, want 1 left for the next pass", n)
	}
	if err := r.sw.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.audioOf(inc.Local) != "B-NEW B24" {
		t.Fatal("Resume replaced a playing track")
	}

	r.fp.playFrom = 0
	if err := r.sw.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.audioOf(inc.Local) != "A-OLD SUSPECT" {
		t.Fatal("Resume did not finish the undo once playback stopped")
	}
	if n := r.v.count(`SELECT COUNT(*) FROM mus_swap_journal WHERE closed_at = 0`); n != 0 {
		t.Errorf("open journal rows = %d after the undo finished", n)
	}
}

// ---- finding 15: item-less placements are finished by the scheduled Resume ----

func TestResumeConfirmsAnItemlessPlacementOncePlexIndexesIt(t *testing.T) {
	r := newSwapHarness(t)
	src := filepath.Join(r.pr.stage, "album", "01.flac")
	tags := TrackTags{Artist: "Band", Album: "Album", Title: "Track One", Disc: 1, Track: 1}
	sha := placeWrite(t, src, "ALBUM-TRACK-ONE", tags)
	dest := filepath.Join(r.pr.lib, "Band", "Album", "01. Track One.flac")
	r.fp.hide = true // Plex has not indexed the folder yet

	row, err := r.pl.Stage(context.Background(), StageSpec{
		Kind: JournalPlace, Source: src, SourceSHA: sha, Dest: dest, ExpectFingerprint: placeFP("ALBUM-TRACK-ONE"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pl.Advance(context.Background(), row); err != nil {
		t.Fatal(err)
	}

	if err := r.sw.Resume(context.Background()); err != nil {
		t.Fatalf("resume while Plex lags: %v", err)
	}
	cur, _ := r.pl.jr.Get(context.Background(), row.ID)
	if !cur.Open() || cur.Phase != PhasePublished {
		t.Fatalf("row open=%v phase=%d after a Resume Plex could not satisfy, want open at published (pending)", cur.Open(), cur.Phase)
	}

	r.fp.hide = false
	if err := r.sw.Resume(context.Background()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	cur, _ = r.pl.jr.Get(context.Background(), row.ID)
	if cur.Open() || cur.Phase != PhasePlexConfirmed {
		t.Fatalf("row open=%v phase=%d, want confirmed and closed", cur.Open(), cur.Phase)
	}
	if n := r.v.count(`SELECT COUNT(*) FROM mus_library_tracks WHERE path = ?`, r.fp.toPlex(dest)); n != 1 {
		t.Errorf("library tracks for the placed file = %d, want 1", n)
	}
}

// A song downloaded as a track of a various-artists compilation is placed under the pinned
// recording's own release, numbered as on it, with the album tags rewritten (Beast,
// 2026-10-15: two songs landed in "Various Artists/NOW That's What I Call Music").
func TestCompilationRipIsPlacedUnderTheRecordingsOwnRelease(t *testing.T) {
	r := newPlaceRig(t)
	s := r.seed(placeSeed{Title: "Leave Before You Love Me", Artist: "Marshmello & Jonas Brothers",
		Album: "NOW 79", AlbumArtist: "Various Artists", Disc: 1, Track: 16})
	rel := &metadata.ReleaseDetails{Artist: "Marshmello & Jonas Brothers", Tracks: []metadata.ReleaseTrack{
		{RecordingMBID: "other", Disc: 1, Position: 1}, {RecordingMBID: "mbid-Leave Before You Love Me", Disc: 1, Position: 2}}}
	rel.MBID, rel.Title, rel.Date, rel.ReleaseGroupMBID, rel.PrimaryType = "own-rel", "Shockwave", "2021-06-11", "own-rg", "Album"
	r.pl.cfg.Releases = placeFakeReleases{rel: rel}
	if _, err := r.v.db.Exec(`UPDATE mus_recordings SET release_mbid = 'own-rel' WHERE id = ?`, s.RecID); err != nil {
		t.Fatal(err)
	}
	r.drive(s.ItemID, 5)

	got := r.libFlacs()
	want := filepath.Join("Marshmello & Jonas Brothers", "Shockwave")
	if len(got) != 1 || !strings.Contains(got[0], want) || !strings.Contains(filepath.Base(got[0]), "02") {
		t.Fatalf("library = %v, want track 02 under %s", got, want)
	}
	b, err := os.ReadFile(got[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, tags, _ := placeDecode(b); tags.Album != "Shockwave" || tags.AlbumArtist != "Marshmello & Jonas Brothers" || tags.Compilation {
		t.Fatalf("tags = %+v, want the own release", tags)
	}
}

// A compilation rip whose pinned release is itself a compilation keeps its own tags.
func TestCompilationRipStaysWhenThePinnedReleaseIsACompilation(t *testing.T) {
	r := newPlaceRig(t)
	s := r.seed(placeSeed{Title: "Song", Artist: "Band", Album: "Hits", AlbumArtist: "Various Artists", Disc: 1, Track: 3})
	rel := &metadata.ReleaseDetails{Tracks: []metadata.ReleaseTrack{{RecordingMBID: "mbid-Song", Disc: 1, Position: 3}}}
	rel.MBID, rel.Title, rel.SecondaryTypes = "comp", "Hits", []string{"Compilation"}
	r.pl.cfg.Releases = placeFakeReleases{rel: rel}
	if _, err := r.v.db.Exec(`UPDATE mus_recordings SET release_mbid = 'comp' WHERE id = ?`, s.RecID); err != nil {
		t.Fatal(err)
	}
	r.drive(s.ItemID, 5)
	if got := r.libFlacs(); len(got) != 1 || !strings.Contains(got[0], filepath.Join("Various Artists", "Hits")) {
		t.Fatalf("library = %v, want it left under Various Artists/Hits", got)
	}
}
