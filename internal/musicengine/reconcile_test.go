package musicengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// snapshot captures everything a second reconcile must leave unchanged: the library and
// quarantine trees (checksum and mode), every journal row's phase and state, and every item.
func (r *placeRig) snapshot() string {
	r.t.Helper()
	out := map[string]any{"lib": placeTree(r.t, r.lib), "quar": placeTree(r.t, r.quar)}
	rows, err := r.v.db.Query(`SELECT id, kind, phase, closed_at > 0, last_error, temp_sha256, published_sha256 FROM mus_swap_journal ORDER BY id`)
	if err != nil {
		r.t.Fatal(err)
	}
	var journal []string
	for rows.Next() {
		var id int64
		var kind, lastErr, tsha, psha string
		var phase int
		var closed bool
		if err := rows.Scan(&id, &kind, &phase, &closed, &lastErr, &tsha, &psha); err != nil {
			r.t.Fatal(err)
		}
		journal = append(journal, fmt.Sprintf("%d %s p%d closed=%v err=%q %s %s", id, kind, phase, closed, lastErr, tsha, psha))
	}
	rows.Close()
	out["journal"] = journal
	irows, err := r.v.db.Query(`SELECT id, state, last_reason FROM mus_items ORDER BY id`)
	if err != nil {
		r.t.Fatal(err)
	}
	var items []string
	for irows.Next() {
		var id int64
		var st, why string
		_ = irows.Scan(&id, &st, &why)
		items = append(items, fmt.Sprintf("%d %s %s", id, st, why))
	}
	irows.Close()
	out["items"] = items
	b, _ := json.Marshal(out)
	return string(b)
}

func (r *placeRig) mustReconcile() ReconcileReport {
	r.t.Helper()
	rep, err := r.pl.Reconcile(context.Background())
	if err != nil {
		r.t.Fatalf("reconcile: %v", err)
	}
	return rep
}

func (r *placeRig) openRows() int {
	return r.v.count(`SELECT COUNT(*) FROM mus_swap_journal WHERE closed_at = 0`)
}

func TestReconcileNewTrackAtEveryCrashPoint(t *testing.T) {
	points := []string{
		"stage:begin",         // row opened, nothing else (before staged is recorded)
		"stage:staged",        // after phase 1 recorded
		"stage:mid_copy",      // half-written temp file
		"stage:copied",        // temp complete, untagged
		"stage:tagged",        // tagged, before phase 2 is recorded
		"stage:temp_verified", // after phase 2 recorded
		"place:linked",        // final name published, phase 4 not recorded (mid publish)
		"place:temp_removed",  // temp removed, phase 4 not recorded
		"place:recorded",      // after phase 4 recorded
	}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			r := newPlaceRig(t)
			s := r.seed(placeSeed{Title: "Song", Artist: "Band", Album: "Album", Disc: 1, Track: 1, Audio: "AUDIO-ONE"})
			r.pl.crash = func(p string) bool { return p == point }
			err := r.step(s.ItemID)
			if !errors.Is(err, errCrash) {
				t.Fatalf("step err = %v, want the simulated crash at %s", err, point)
			}

			r.restart()
			rep := r.mustReconcile()
			if rep.Rows != 1 {
				t.Fatalf("reconcile saw %d rows, want 1", rep.Rows)
			}
			if left := placeTemps(t, r.lib); len(left) != 0 {
				t.Errorf("temp files after reconcile: %v", left)
			}
			if got := r.libFlacs(); len(got) > 1 {
				t.Errorf("library holds %d copies after reconcile: %v", len(got), got)
			}
			for _, f := range r.libFlacs() {
				b, _ := os.ReadFile(f)
				if audio, _, err := placeDecode(b); err != nil || audio != "AUDIO-ONE" {
					t.Errorf("%s is not a complete copy of the audio: %q %v", f, audio, err)
				}
			}
			first := r.snapshot()

			// A second reconcile over the same rows changes nothing (AE9).
			r.mustReconcile()
			if second := r.snapshot(); second != first {
				t.Errorf("second reconcile changed state:\nfirst : %s\nsecond: %s", first, second)
			}

			// And the item still finishes: exactly one complete copy, every row settled.
			r.drive(s.ItemID, 8)
			files := r.libFlacs()
			if len(files) != 1 {
				t.Fatalf("library = %v, want exactly one file", files)
			}
			want := filepath.Join(r.lib, "Band", "Album", "01. Song.flac")
			if files[0] != want {
				t.Errorf("file at %s, want %s", files[0], want)
			}
			if left := placeTemps(t, r.lib); len(left) != 0 {
				t.Errorf("temp files at the end: %v", left)
			}
			if n := r.openRows(); n != 0 {
				t.Errorf("%d journal rows still open at the end", n)
			}
		})
	}
}

// swapRig prepares an incumbent in the library and a verified-by-acquire new file in staging.
type swapRig struct {
	*placeRig
	dest, newSrc, quarPath string
	oldSHA, newSHA         string
	itemID                 int64
}

func newSwapRig(t *testing.T) *swapRig {
	r := newPlaceRig(t)
	s := &swapRig{placeRig: r}
	s.dest = filepath.Join(r.lib, "Band", "Album", "01. Song.flac")
	tags := TrackTags{Artist: "Band", Album: "Album", Title: "Song", Disc: 1, Track: 1}
	s.oldSHA = placeWrite(t, s.dest, "OLD-AUDIO", tags)
	s.newSrc = filepath.Join(r.stage, "swap", "song.flac")
	s.newSHA = placeWrite(t, s.newSrc, "NEW-AUDIO", tags)
	s.quarPath = filepath.Join(r.quar, "track-1", "01. Song.old.flac")
	rec := r.v.seedRecording("mb-swap", "Song", "Band", "", "studio")
	s.itemID = r.v.seedItem(KindUpgrade, StateSwapping, rec)
	return s
}

func (s *swapRig) spec() StageSpec {
	return StageSpec{
		Kind: JournalSwap, ItemID: s.itemID, Source: s.newSrc, SourceSHA: s.newSHA, Dest: s.dest,
		IncumbentSHA: s.oldSHA, QuarantinePath: s.quarPath, ExpectFingerprint: placeFP("NEW-AUDIO"),
	}
}

func (s *swapRig) run() error {
	row, err := s.pl.Stage(context.Background(), s.spec())
	if err != nil {
		return err
	}
	_, err = s.pl.Advance(context.Background(), row)
	return err
}

func TestReconcileSwapAtEveryCrashPoint(t *testing.T) {
	points := []string{
		"stage:begin", "stage:staged", "stage:mid_copy", "stage:copied", "stage:tagged", "stage:temp_verified",
		"quarantine:mid_copy", // partial quarantine copy
		"quarantine:copied",   // complete copy, phase 3 not recorded
		"quarantine:recorded", // after phase 3 recorded
		"swap:renamed",        // new file in place, phase 4 not recorded
		"place:temp_removed",
		"place:recorded", // after phase 4 recorded
	}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			s := newSwapRig(t)
			s.pl.crash = func(p string) bool { return p == point }
			if err := s.run(); !errors.Is(err, errCrash) {
				t.Fatalf("run err = %v, want the simulated crash at %s", err, point)
			}
			s.restart()
			s.mustReconcile()

			if left := placeTemps(t, s.lib); len(left) != 0 {
				t.Errorf("temp files after reconcile: %v", left)
			}
			got, err := placeSHA256(s.dest)
			if err != nil {
				t.Fatalf("destination unreadable after reconcile: %v", err)
			}
			switch got {
			case s.oldSHA:
				// rolled back: the incumbent is whole and the row is settled
				if n := s.openRows(); n != 0 {
					t.Errorf("incumbent in place but %d rows open", n)
				}
			case s.newSHA:
				q, err := placeSHA256(s.quarPath)
				if err != nil || q != s.oldSHA {
					t.Errorf("new file in place but the incumbent is not safe in quarantine (%v, %s)", err, q)
				}
				if n := s.v.count(`SELECT COUNT(*) FROM mus_quarantine WHERE path = ? AND sha256 = ?`, s.quarPath, s.oldSHA); n != 1 {
					t.Errorf("quarantine rows = %d, want 1", n)
				}
				if it := s.v.item(s.itemID); it.State != StateAwaitingPlexIndex {
					t.Errorf("item = %s, want awaiting_plex_index", it.State)
				}
			default:
				t.Fatalf("destination holds neither version (sha %s)", got)
			}
			if files := s.libFlacs(); len(files) != 1 {
				t.Errorf("library holds %v, want exactly one file", files)
			}
			first := s.snapshot()
			s.mustReconcile()
			if second := s.snapshot(); second != first {
				t.Errorf("second reconcile changed state:\nfirst : %s\nsecond: %s", first, second)
			}
		})
	}
}

func TestSwapStopsWhenDestinationNoLongerHoldsTheIncumbent(t *testing.T) {
	s := newSwapRig(t)
	row, err := s.pl.Stage(context.Background(), s.spec())
	if err != nil {
		t.Fatal(err)
	}
	// Someone replaces the incumbent between staging and publishing (KTD18).
	otherSHA := placeWrite(t, s.dest, "SOMEONE-ELSE", TrackTags{Artist: "Band", Album: "Album", Title: "Song", Disc: 1, Track: 1})
	out, err := s.pl.Advance(context.Background(), row)
	if err != nil || out != OutcomeStopped {
		t.Fatalf("Advance = %v %v, want stopped", out, err)
	}
	if got, _ := placeSHA256(s.dest); got != otherSHA {
		t.Error("the destination was changed")
	}
	if _, err := os.Stat(s.quarPath); err == nil {
		t.Error("something was quarantined for a swap that did not happen")
	}
	if left := placeTemps(t, s.lib); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
	if n := s.v.count(`SELECT COUNT(*) FROM mus_decisions WHERE item_id = ? AND kind = 'collision' AND answered_at = 0`, s.itemID); n != 1 {
		t.Errorf("collision decisions = %d, want 1", n)
	}
	if it := s.v.item(s.itemID); it.State != StateWaitingBlair {
		t.Errorf("item = %s, want waiting_blair", it.State)
	}
}

func TestSwapThenUndoRunThroughTheSameJournal(t *testing.T) {
	s := newSwapRig(t)
	var finalized []string
	s.pl.cfg.Finalize = func(_ context.Context, row JournalRow, key string) error {
		finalized = append(finalized, row.Kind+":"+key)
		return nil
	}
	if err := s.run(); err != nil {
		t.Fatal(err)
	}
	if got, _ := placeSHA256(s.dest); got != s.newSHA {
		t.Fatal("swap did not publish the new file")
	}
	s.drive(s.itemID, 3) // awaiting_plex_index -> Finalize -> done
	if len(finalized) != 1 || !strings.HasPrefix(finalized[0], "swap:rk") {
		t.Errorf("finalize calls = %v, want one swap call with Plex's key", finalized)
	}
	if q, _ := placeSHA256(s.quarPath); q != s.oldSHA {
		t.Error("the old version is not in quarantine")
	}

	// Undo: the quarantined version is the incoming file, the current file is the incumbent.
	undoQuar := filepath.Join(s.quar, "track-1", "01. Song.new.flac")
	row, err := s.pl.Stage(context.Background(), StageSpec{
		Kind: JournalUndo, Source: s.quarPath, SourceSHA: s.oldSHA, Dest: s.dest, IncumbentSHA: s.newSHA,
		QuarantinePath: undoQuar, ExpectFingerprint: placeFP("OLD-AUDIO"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pl.Advance(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if got, _ := placeSHA256(s.dest); got != s.oldSHA {
		t.Error("undo did not restore the old version")
	}
	if q, _ := placeSHA256(undoQuar); q != s.newSHA {
		t.Error("undo did not quarantine the version it replaced")
	}
	// The row has no item, so the reconciler confirms it with Plex once.
	rep := s.mustReconcile()
	if rep.ConfirmedBy != 1 || s.openRows() != 0 {
		t.Errorf("reconcile report %+v, open rows %d: want the undo row confirmed and closed", rep, s.openRows())
	}
}

func TestReconcileLeavesRowsUntouchedWhileLibraryIsUnreachable(t *testing.T) {
	r := newPlaceRig(t)
	s := r.seed(placeSeed{Title: "Song", Artist: "Band", Album: "Album", Disc: 1, Track: 1})
	r.pl.crash = func(p string) bool { return p == "stage:copied" }
	if err := r.step(s.ItemID); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	r.restart()
	before := r.snapshot()
	r.measOK = false
	if _, err := r.pl.Reconcile(context.Background()); !errors.Is(err, ErrLibraryUnreachable) {
		t.Fatalf("reconcile err = %v, want ErrLibraryUnreachable", err)
	}
	r.measOK = true
	if after := r.snapshot(); after != before {
		t.Error("reconcile changed state while the library was unreachable")
	}
	if n := r.openRows(); n != 1 {
		t.Errorf("open rows = %d, want the row kept", n)
	}
}

func TestPlaceBeginRefusesSecondOpenRowOnOnePath(t *testing.T) {
	r := newPlaceRig(t)
	s := r.seed(placeSeed{Title: "Song", Artist: "Band", Album: "Album", Disc: 1, Track: 1})
	r.pl.crash = func(p string) bool { return p == "stage:copied" }
	if err := r.step(s.ItemID); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	dest := filepath.Join(r.lib, "Band", "Album", "01. Song.flac")
	_, err := r.pl.Journal().Start(context.Background(), JournalBegin{Kind: JournalSwap, NewPath: dest, OldPath: dest})
	if !errors.Is(err, ErrJournalBusy) {
		t.Errorf("second Begin err = %v, want ErrJournalBusy", err)
	}
}
