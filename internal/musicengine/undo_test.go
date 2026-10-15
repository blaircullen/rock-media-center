package musicengine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func (r *swapHarness) latestSwap() int64 {
	r.t.Helper()
	var id int64
	if err := r.v.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM mus_swap_journal WHERE kind = 'swap' AND phase = 5`).Scan(&id); err != nil || id == 0 {
		r.t.Fatalf("no completed swap (%v)", err)
	}
	return id
}

// swapTo runs one full swap of the track to a new candidate and returns its journal id.
func (r *swapHarness) swapTo(inc swapInc, audio string) int64 {
	r.t.Helper()
	item, _ := r.seedSwap(inc, swapCandOpts{Audio: audio})
	r.drive(item, 8, StateDone)
	return r.latestSwap()
}

func (r *swapHarness) mustUndo(swapID int64) UndoResult {
	r.t.Helper()
	res, err := r.sw.Undo(context.Background(), swapID)
	if err != nil {
		r.t.Fatalf("undo of swap %d: %v", swapID, err)
	}
	return res
}

func (r *swapHarness) wantRefusal(swapID int64) *UndoRefusal {
	r.t.Helper()
	_, err := r.sw.Undo(context.Background(), swapID)
	var ref *UndoRefusal
	if !errors.As(err, &ref) {
		r.t.Fatalf("undo of swap %d: err = %v, want a refusal", swapID, err)
	}
	if !errors.Is(err, ErrUndoRefused) {
		r.t.Errorf("refusal %v does not match ErrUndoRefused", err)
	}
	return ref
}

// AE10: swap 1 replaced A with B and swap 2 replaced B with C. Undoing swap 1 is refused until
// swap 2 is undone; after both, B never returns for the track.
func TestUndoRefusedWhileALaterSwapExistsThenBlocksTheCandidate(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "A-OLD SUSPECT", RG: "rg1", Rating: 6, Playlists: []string{"Road trip"}})
	shaA := r.sha(inc.Local)
	swap1 := r.swapTo(inc, "B-MIDDLE B16")
	shaB := r.sha(inc.Local)
	swap2 := r.swapTo(inc, "C-NEWEST B24")
	if r.audioOf(inc.Local) != "C-NEWEST B24" {
		t.Fatalf("library holds %q, want C after two swaps", r.audioOf(inc.Local))
	}

	ref := r.wantRefusal(swap1)
	if ref.LaterSwapID != swap2 {
		t.Errorf("refusal later_swap_id = %d, want %d", ref.LaterSwapID, swap2)
	}
	if !strings.Contains(ref.Reason, fmt.Sprintf("undo swap %d first", swap2)) {
		t.Errorf("reason %q should tell Blair to undo swap %d first", ref.Reason, swap2)
	}
	if r.audioOf(inc.Local) != "C-NEWEST B24" || r.journalRows(JournalUndo) != 0 {
		t.Fatal("the refused undo changed something")
	}

	res := r.mustUndo(swap2)
	if !res.Confirmed || res.UndoID == 0 {
		t.Fatalf("undo of swap 2 = %+v, want confirmed", res)
	}
	if r.audioOf(inc.Local) != "B-MIDDLE B16" || r.sha(inc.Local) != shaB {
		t.Fatalf("after undoing swap 2 the library holds %q (sha match %v), want B back byte for byte", r.audioOf(inc.Local), r.sha(inc.Local) == shaB)
	}
	// Swap 2's candidate (C) is blocked; the swap row says it was undone, and by what.
	var undoneAt, undoneBy int64
	if err := r.v.db.QueryRow(`SELECT undone_at, COALESCE(undone_by_id, 0) FROM mus_swap_journal WHERE id = ?`, swap2).Scan(&undoneAt, &undoneBy); err != nil || undoneAt == 0 || undoneBy != res.UndoID {
		t.Fatalf("swap 2 undone_at %d undone_by %d (%v), want it marked by undo %d", undoneAt, undoneBy, err, res.UndoID)
	}
	if n := r.v.count(`SELECT COUNT(*) FROM mus_blocked_candidates WHERE recording_id = ? AND pcm_fingerprint = ?`, inc.RecID, placeFP("C-NEWEST B24")); n != 1 {
		t.Errorf("blocked rows for C = %d, want 1", n)
	}

	// Now swap 1 can be undone.
	r.mustUndo(swap1)
	if r.audioOf(inc.Local) != "A-OLD SUSPECT" || r.sha(inc.Local) != shaA {
		t.Fatalf("after undoing swap 1 the library holds %q, want A back byte for byte", r.audioOf(inc.Local))
	}
	if n := r.v.count(`SELECT COUNT(*) FROM mus_blocked_candidates WHERE recording_id = ? AND pcm_fingerprint = ?`, inc.RecID, placeFP("B-MIDDLE B16")); n != 1 {
		t.Errorf("blocked rows for B (the swap-1 candidate) = %d, want 1", n)
	}

	// B never returns: offered again, it is kept out even though it is an upgrade over A.
	item, _ := r.seedSwap(inc, swapCandOpts{Audio: "B-MIDDLE B16"})
	r.mustStep(item)
	it := r.v.item(item)
	if it.State != StateDropped || !strings.Contains(it.LastReason, "blocked") {
		t.Fatalf("re-offered B: item %s (%q), want dropped as blocked", it.State, it.LastReason)
	}
	if r.audioOf(inc.Local) != "A-OLD SUSPECT" {
		t.Fatal("a blocked candidate came back")
	}
	// An undone swap cannot be undone twice.
	if ref := r.wantRefusal(swap1); !strings.Contains(ref.Reason, "already undone") {
		t.Errorf("second undo reason %q", ref.Reason)
	}
}

// An undo is itself a journaled, quarantining change: nothing is lost, the rating and
// playlists survive, and the replaced file lands in quarantine as one more version.
func TestUndoQuarantinesTheCurrentFileAndKeepsRatingAndPlaylists(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "A-OLD SUSPECT", RG: "rg1", Rating: 8, Playlists: []string{"Road trip"}})
	swap1 := r.swapTo(inc, "B-NEW B24")
	shaB := r.sha(inc.Local)

	r.mustUndo(swap1)

	if n := r.v.count(`SELECT COUNT(*) FROM mus_quarantine WHERE cleared_at = 0`); n != 2 {
		t.Fatalf("quarantine holds %d versions, want 2 (A from the swap, B from the undo)", n)
	}
	found := false
	rows, err := r.v.db.Query(`SELECT path, sha256 FROM mus_quarantine`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var p, s string
		_ = rows.Scan(&p, &s)
		if s == shaB && r.sha(p) == shaB && strings.Contains(p, fmt.Sprintf("undo-%d-", swap1)) {
			found = true
		}
	}
	rows.Close()
	if !found {
		t.Error("the file the undo replaced (B) is not in quarantine under the undo's name")
	}
	key := r.keyOf(inc.TrackID)
	if got := r.ratingOf(key); got != 8 {
		t.Errorf("rating after undo = %v, want 8", got)
	}
	if !r.inPlaylist("Road trip", key) {
		t.Error("playlist membership lost by the undo")
	}
	var kind string
	var phase int
	var prev int64
	if err := r.v.db.QueryRow(`SELECT kind, phase, previous_swap_id FROM mus_swap_journal WHERE kind = 'undo'`).Scan(&kind, &phase, &prev); err != nil || phase != PhasePlexConfirmed || prev != swap1 {
		t.Errorf("undo journal row: kind %s phase %d previous %d (%v)", kind, phase, prev, err)
	}
	// The library track carries the restored file's facts again.
	var verdict string
	if err := r.v.db.QueryRow(`SELECT verdict FROM mus_library_tracks WHERE id = ?`, inc.TrackID).Scan(&verdict); err != nil || verdict != "SUSPECT" {
		t.Errorf("library track verdict after undo = %q (%v), want the restored file's SUSPECT", verdict, err)
	}
	// The undone swap is gone from the digest's list of undoable swaps.
	d, err := NewDigest(r.v.db, r.v.notifier, r.v.clock.Now).Build(context.Background(), r.v.clock.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Swaps) != 0 {
		t.Errorf("digest lists %d swaps after the undo, want 0", len(d.Swaps))
	}
}

func TestUndoRefusesWhenTheLibraryFileIsNoLongerTheSwapsVersion(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "A-OLD SUSPECT", RG: "rg1"})
	swap1 := r.swapTo(inc, "B-NEW B24")
	placeWrite(t, inc.Local, "HAND-EDITED", TrackTags{Artist: "Band", Album: "Album", Title: "Gold", Track: 1, RecordingMBID: inc.MBID})
	edited := r.sha(inc.Local)
	ref := r.wantRefusal(swap1)
	if !strings.Contains(ref.Reason, "is no longer the one swap") {
		t.Errorf("reason %q should say the file is not the swap's version", ref.Reason)
	}
	if r.sha(inc.Local) != edited || r.journalRows(JournalUndo) != 0 {
		t.Fatal("the refused undo overwrote a hand-edited file")
	}
}

func TestUndoRefusesWhileTheTrackIsPlaying(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "A-OLD SUSPECT", RG: "rg1"})
	swap1 := r.swapTo(inc, "B-NEW B24")
	r.fp.playing[r.keyOf(inc.TrackID)] = true
	if ref := r.wantRefusal(swap1); !strings.Contains(ref.Reason, "playing") {
		t.Errorf("reason %q should say the track is playing", ref.Reason)
	}
	if r.audioOf(inc.Local) != "B-NEW B24" {
		t.Fatal("the file changed under playback")
	}
	r.fp.playing[r.keyOf(inc.TrackID)] = false
	r.mustUndo(swap1)
	if r.audioOf(inc.Local) != "A-OLD SUSPECT" {
		t.Error("the undo did not run once playback stopped")
	}
}

func TestUndoRefusesWhatIsNotACompletedSwap(t *testing.T) {
	r := newSwapHarness(t)
	if ref := r.wantRefusal(999); !strings.Contains(ref.Reason, "doesn't exist") {
		t.Errorf("unknown id: %q", ref.Reason)
	}
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "A-OLD SUSPECT", RG: "rg1"})
	item, _ := r.seedSwap(inc, swapCandOpts{Audio: "B-NEW B24"})
	r.pl.crash = func(pt string) bool { return pt == "stage:temp_verified" }
	_ = r.step(item)
	r.pl.crash = nil
	var rowID int64
	if err := r.v.db.QueryRow(`SELECT id FROM mus_swap_journal`).Scan(&rowID); err != nil {
		t.Fatal(err)
	}
	if ref := r.wantRefusal(rowID); !strings.Contains(ref.Reason, "not a completed swap") {
		t.Errorf("open swap: %q", ref.Reason)
	}
}

// The quarantined file is checked before it is trusted: a missing or altered one refuses.
func TestUndoRefusesWhenTheQuarantinedFileIsMissingOrAltered(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "A-OLD SUSPECT", RG: "rg1"})
	swap1 := r.swapTo(inc, "B-NEW B24")
	var qp string
	if err := r.v.db.QueryRow(`SELECT path FROM mus_quarantine`).Scan(&qp); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(qp, []byte("bitrot"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ref := r.wantRefusal(swap1); !strings.Contains(ref.Reason, "has changed since it was saved") {
		t.Errorf("altered: %q", ref.Reason)
	}
	if err := os.Remove(qp); err != nil {
		t.Fatal(err)
	}
	if ref := r.wantRefusal(swap1); !strings.Contains(ref.Reason, "missing") {
		t.Errorf("missing: %q", ref.Reason)
	}
	if r.audioOf(inc.Local) != "B-NEW B24" {
		t.Fatal("the library changed")
	}
}

// If Plex has not indexed the restored file yet, the undo reports that, the file is back in
// place, and the sweep's Resume finishes it: the swap is marked undone and the candidate blocked.
func TestUndoFinishedByResumeWhenPlexLags(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "A-OLD SUSPECT", RG: "rg1", Rating: 6})
	swap1 := r.swapTo(inc, "B-NEW B24")
	r.fp.onRefresh = func() { r.fp.hide = true } // Plex lists nothing once the folder is refreshed

	res := r.mustUndo(swap1)
	if res.Confirmed {
		t.Fatal("undo confirmed although Plex listed nothing")
	}
	if r.audioOf(inc.Local) != "A-OLD SUSPECT" {
		t.Fatal("the file is not back in place")
	}
	var undone int64
	_ = r.v.db.QueryRow(`SELECT undone_at FROM mus_swap_journal WHERE id = ?`, swap1).Scan(&undone)
	if undone != 0 {
		t.Fatal("the swap was marked undone before Plex confirmed the restore")
	}
	if err := r.sw.Resume(context.Background()); err != nil {
		t.Fatalf("resume while Plex lags: %v", err)
	}
	r.fp.hide, r.fp.onRefresh = false, nil
	if err := r.sw.Resume(context.Background()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	_ = r.v.db.QueryRow(`SELECT undone_at FROM mus_swap_journal WHERE id = ?`, swap1).Scan(&undone)
	if undone == 0 {
		t.Fatal("Resume did not finish the undo")
	}
	if n := r.v.count(`SELECT COUNT(*) FROM mus_blocked_candidates WHERE pcm_fingerprint = ?`, placeFP("B-NEW B24")); n != 1 {
		t.Errorf("blocked rows = %d, want 1", n)
	}
	if n := r.v.count(`SELECT COUNT(*) FROM mus_swap_journal WHERE closed_at = 0`); n != 0 {
		t.Errorf("open journal rows = %d", n)
	}
	if got := r.ratingOf(r.keyOf(inc.TrackID)); got != 6 {
		t.Errorf("rating = %v, want 6", got)
	}
}

// A crash during an undo is settled by the boot reconciler like any journal row; Resume then
// marks the swap undone and blocks the candidate.
func TestUndoCrashSettledByReconcileThenResume(t *testing.T) {
	for _, point := range []string{"stage:temp_verified", "quarantine:copied", "swap:renamed"} {
		t.Run(point, func(t *testing.T) {
			r := newSwapHarness(t)
			inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "A-OLD SUSPECT", RG: "rg1"})
			swap1 := r.swapTo(inc, "B-NEW B24")
			shaB := r.sha(inc.Local)

			r.pl.crash = func(pt string) bool { return pt == point }
			if _, err := r.sw.Undo(context.Background(), swap1); !errors.Is(err, errCrash) {
				t.Fatalf("undo err = %v, want the simulated crash", err)
			}
			r.build()
			if _, err := r.pl.Reconcile(context.Background()); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			before := r.pr.snapshot()
			if _, err := r.pl.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			if r.pr.snapshot() != before {
				t.Error("a second reconcile changed state")
			}
			if flacs := r.pr.libFlacs(); len(flacs) != 1 {
				t.Fatalf("library flacs = %v, want exactly one", flacs)
			}
			if err := r.sw.Resume(context.Background()); err != nil {
				t.Fatalf("resume: %v", err)
			}
			if n := r.v.count(`SELECT COUNT(*) FROM mus_swap_journal WHERE closed_at = 0`); n != 0 {
				t.Fatalf("open journal rows = %d after resume", n)
			}
			if r.audioOf(inc.Local) != "A-OLD SUSPECT" {
				t.Fatalf("library holds %q, want A restored", r.audioOf(inc.Local))
			}
			var undone int64
			_ = r.v.db.QueryRow(`SELECT undone_at FROM mus_swap_journal WHERE id = ?`, swap1).Scan(&undone)
			if undone == 0 {
				t.Error("the swap was not marked undone")
			}
			if n := r.v.count(`SELECT COUNT(*) FROM mus_blocked_candidates WHERE pcm_fingerprint = ?`, placeFP("B-NEW B24")); n != 1 {
				t.Errorf("blocked rows = %d, want 1", n)
			}
			// B (the file the undo replaced) is in quarantine whole.
			ok := false
			rows, err := r.v.db.Query(`SELECT path, sha256 FROM mus_quarantine`)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var p, s string
				_ = rows.Scan(&p, &s)
				if s == shaB && r.sha(p) == shaB {
					ok = true
				}
			}
			rows.Close()
			if !ok {
				t.Error("B is not in quarantine intact")
			}
		})
	}
}
