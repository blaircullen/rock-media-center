package musicengine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// decWaiting puts a seeded item into waiting_blair with an open decision, as the core and
// U9/U11 do, and returns the decision id.
func (v *env) decWaiting(itemID int64, kind string, candidateID int64, options ...string) int64 {
	v.t.Helper()
	if _, err := v.db.Exec(`UPDATE mus_items SET state = 'waiting_blair', deadline_at = 0 WHERE id = ?`, itemID); err != nil {
		v.t.Fatal(err)
	}
	if err := (tableDecisions{}).Raise(context.Background(), v.db, DecisionRequest{
		ItemID: itemID, Kind: kind, Reason: "test " + kind, Options: options, Now: v.clock.Now(),
	}); err != nil {
		v.t.Fatal(err)
	}
	var id int64
	if err := v.db.QueryRow(`SELECT id FROM mus_decisions WHERE item_id = ? AND kind = ?`, itemID, kind).Scan(&id); err != nil {
		v.t.Fatal(err)
	}
	if candidateID != 0 {
		if _, err := v.db.Exec(`UPDATE mus_decisions SET candidate_id = ? WHERE id = ?`, candidateID, id); err != nil {
			v.t.Fatal(err)
		}
	}
	return id
}

func (v *env) decCandidate(itemID int64, stagingPath string, expiresAt int64) int64 {
	v.t.Helper()
	res, err := v.db.Exec(`INSERT INTO mus_candidates (item_id, lane, state, staging_path, expires_at, created_at) VALUES (?, 'youtube', 'checked', ?, ?, ?)`,
		itemID, stagingPath, expiresAt, v.clock.Now().Unix())
	if err != nil {
		v.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (v *env) decAnswered(id int64) (answer, note string, answeredAt int64) {
	v.t.Helper()
	if err := v.db.QueryRow(`SELECT answer, answer_note, answered_at FROM mus_decisions WHERE id = ?`, id).Scan(&answer, &note, &answeredAt); err != nil {
		v.t.Fatal(err)
	}
	return
}

func (v *env) nextAttemptAt(itemID int64) int64 {
	v.t.Helper()
	var n int64
	if err := v.db.QueryRow(`SELECT next_attempt_at FROM mus_items WHERE id = ?`, itemID).Scan(&n); err != nil {
		v.t.Fatal(err)
	}
	return n
}

var decNoGenuine = []string{OptionLossy, OptionWait, OptionDrop}

// "Keep waiting" for a candidate that expired from staging starts a new search at once.
func TestAnswerWaitForExpiredCandidateStartsANewSearch(t *testing.T) {
	v := newEnv(t)
	d := NewDecisions(v.e, AnswerHooks{})
	item := v.seedItem(KindRequest, StateSearching, 0)
	gone := v.decCandidate(item, filepath.Join(t.TempDir(), "never-existed"), 0)
	id := v.decWaiting(item, "no_genuine_copy", gone, decNoGenuine...)

	res, err := d.Answer(context.Background(), id, OptionWait, "")
	if err != nil {
		t.Fatal(err)
	}
	it := v.item(item)
	if res.Ignored || it.State != StateSearching || !strings.Contains(it.LastReason, "the search starts again") {
		t.Fatalf("result %+v item %s %q, want a new search with the reason", res, it.State, it.LastReason)
	}
	if v.nextAttemptAt(item) != 0 {
		t.Error("a new search for an expired candidate should start now, not be held")
	}
	if ans, _, at := v.decAnswered(id); ans != OptionWait || at == 0 {
		t.Errorf("decision not recorded as answered: %q %d", ans, at)
	}
}

// With a candidate still in staging, "keep waiting" searches again but holds the item for
// a day, so a song with no lossless copy yet does not come straight back to the list.
func TestAnswerWaitHoldsTheItemForADay(t *testing.T) {
	v := newEnv(t)
	d := NewDecisions(v.e, AnswerHooks{})
	item := v.seedItem(KindRequest, StateSearching, 0)
	live := filepath.Join(t.TempDir(), "yt.m4a")
	if err := os.WriteFile(live, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cand := v.decCandidate(item, live, v.clock.Now().Add(48*time.Hour).Unix())
	id := v.decWaiting(item, "no_genuine_copy", cand, decNoGenuine...)
	if _, err := d.Answer(context.Background(), id, OptionWait, "later"); err != nil {
		t.Fatal(err)
	}
	if it := v.item(item); it.State != StateSearching {
		t.Fatalf("state %s, want searching", it.State)
	}
	if want := v.clock.Now().Add(DecisionWaitRetry).Unix(); v.nextAttemptAt(item) != want {
		t.Errorf("next attempt %d, want %d (a day on)", v.nextAttemptAt(item), want)
	}
	if _, note, _ := v.decAnswered(id); note != "later" {
		t.Errorf("note = %q", note)
	}
}

// An answer for a song since removed from Spotify is ignored, with the reason kept.
func TestAnswerForASongRemovedFromSpotifyIsIgnored(t *testing.T) {
	v := newEnv(t)
	d := NewDecisions(v.e, AnswerHooks{})
	v.spotify.set(entry("sp1", "Song", "Artist", "I1"), entry("sp2", "Other", "Artist", "I2"))
	v.sync()
	item := v.itemFor("sp1")
	id := v.decWaiting(item.ID, "no_genuine_copy", 0, decNoGenuine...)

	v.spotify.set(entry("sp2", "Other", "Artist", "I2"))
	v.sync()
	if st := v.item(item.ID).State; st != StateCancelled {
		t.Fatalf("setup: removing the song left the item %s, want cancelled", st)
	}

	res, err := d.Answer(context.Background(), id, OptionLossy, "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ignored || !strings.Contains(res.Reason, "removed from the Spotify playlist") {
		t.Fatalf("result %+v, want ignored with the removal reason", res)
	}
	if v.item(item.ID).State != StateCancelled {
		t.Error("an ignored answer must not move the item")
	}
	ans, note, at := v.decAnswered(id)
	if ans != OptionLossy || at == 0 || !strings.HasPrefix(note, "ignored: The song was removed") {
		t.Errorf("decision = %q %q %d, want closed with the reason", ans, note, at)
	}
	open, err := d.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range open {
		if o.ID == id {
			t.Error("an ignored decision must leave the open list")
		}
	}
}

// An item the engine has already moved on is also ignored, with a different reason.
func TestAnswerForAnItemNoLongerWaitingIsIgnored(t *testing.T) {
	v := newEnv(t)
	d := NewDecisions(v.e, AnswerHooks{})
	item := v.seedItem(KindRequest, StateSearching, 0)
	id := v.decWaiting(item, "no_genuine_copy", 0, decNoGenuine...)
	if _, err := v.db.Exec(`UPDATE mus_items SET state = 'placing' WHERE id = ?`, item); err != nil {
		t.Fatal(err)
	}
	res, err := d.Answer(context.Background(), id, OptionDrop, "")
	if err != nil || !res.Ignored || !strings.Contains(res.Reason, "placing") {
		t.Fatalf("result %+v, %v", res, err)
	}
}

// Drop, retry (by the step that raised it) and not-a-match each make their transition and
// close the decision.
func TestAnswerRoutesToTheEngineTransition(t *testing.T) {
	for _, c := range []struct {
		name    string
		kind    string
		options []string
		option  string
		pinned  bool
		want    State
	}{
		{"drop", "no_genuine_copy", decNoGenuine, OptionDrop, false, StateDropped},
		{"retry unpinned ambiguous", "ambiguous", []string{OptionRetry, OptionDrop}, OptionRetry, false, StatePinning},
		{"retry pinned ambiguous", "ambiguous", []string{OptionRetry, OptionDrop}, OptionRetry, true, StateLibraryCheck},
		{"retry blind library", "blind_library", []string{OptionRetry, OptionDrop}, OptionRetry, true, StateLibraryCheck},
		{"not a match", "ambiguous", []string{OptionTreatAsMatch, OptionNotAMatch, OptionDrop}, OptionNotAMatch, true, StateSearching},
		{"keep the incumbent", "different_master", []string{OptionApprove, OptionKeep}, OptionKeep, true, StateDropped},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := newEnv(t)
			d := NewDecisions(v.e, AnswerHooks{})
			var rec int64
			if c.pinned {
				rec = v.seedRecording("rec-x", "Song", "Artist", "", "")
			}
			item := v.seedItem(KindRequest, StateSearching, rec)
			id := v.decWaiting(item, c.kind, 0, c.options...)
			res, err := d.Answer(context.Background(), id, c.option, "")
			if err != nil {
				t.Fatal(err)
			}
			if got := v.item(item).State; got != c.want || res.State != c.want {
				t.Fatalf("item %s (result %s), want %s", got, res.State, c.want)
			}
			if v.openDecisions(item) != 0 {
				t.Error("the decision should be closed")
			}
		})
	}
}

// "use:<mbid>" pins through Engine.PinToRecording and closes the decision.
func TestAnswerUsePinsTheChosenRecording(t *testing.T) {
	v := newEnv(t)
	v.mb.add(mbRec("rec-a", "Song", "Artist", "", "I1"))
	v.mb.add(mbRec("rec-b", "Song", "Artist", "", "I1"))
	v.spotify.set(entry("sp1", "Song", "Artist", "I1"))
	v.sync()
	v.tick(2)
	it := v.itemFor("sp1")
	if it.State != StateWaitingBlair {
		t.Fatalf("setup: %s", it.State)
	}
	d := NewDecisions(v.e, AnswerHooks{})
	open, err := d.Open(context.Background())
	if err != nil || len(open) != 1 || !decHasOption(open[0].Options, "use:rec-b") {
		t.Fatalf("open = %+v, %v", open, err)
	}
	res, err := d.Answer(context.Background(), open[0].ID, "use:rec-b", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := v.item(it.ID); got.State != StateLibraryCheck || v.recordingMBID(got.RecordingID) != "rec-b" || res.State != StateLibraryCheck {
		t.Fatalf("item %s recording %q result %+v", got.State, v.recordingMBID(got.RecordingID), res)
	}
	if _, _, at := v.decAnswered(open[0].ID); at == 0 {
		t.Error("decision should be closed")
	}
}

// Lossy, swap-approve and treat-as-match belong to the lanes and placement: the wiring's
// hook runs; without one the answer fails and changes nothing.
func TestAnswerHooks(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()
	item := v.seedItem(KindRequest, StateSearching, 0)
	staged := filepath.Join(t.TempDir(), "yt.m4a")
	if err := os.WriteFile(staged, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cand := v.decCandidate(item, staged, 0)
	id := v.decWaiting(item, "no_genuine_copy", cand, decNoGenuine...)

	if _, err := NewDecisions(v.e, AnswerHooks{}).Answer(ctx, id, OptionLossy, ""); !errors.Is(err, ErrNoAnswerHook) {
		t.Fatalf("no hook: err = %v, want ErrNoAnswerHook", err)
	}
	if v.item(item).State != StateWaitingBlair || v.openDecisions(item) != 1 {
		t.Fatal("a refused answer must leave the item and the decision as they were")
	}

	var gotDec Decision
	var gotItem Item
	d := NewDecisions(v.e, AnswerHooks{TakeLossy: func(ctx context.Context, dec Decision, it Item) error {
		gotDec, gotItem = dec, it
		return v.e.Transition(ctx, it.ID, StatePlacing, "placing the lossy copy")
	}})
	res, err := d.Answer(ctx, id, OptionLossy, "")
	if err != nil {
		t.Fatal(err)
	}
	if gotDec.ID != id || gotDec.CandidateID != cand || gotItem.ID != item || res.State != StatePlacing {
		t.Fatalf("hook saw decision %+v item %+v; result %+v", gotDec, gotItem, res)
	}
	if _, _, at := v.decAnswered(id); at == 0 {
		t.Error("decision should be closed after the hook succeeded")
	}

	// A hook that fails leaves the decision open.
	item2 := v.seedItem(KindUpgrade, StateSearching, v.seedRecording("r2", "S", "A", "", ""))
	id2 := v.decWaiting(item2, "different_master", 0, OptionApprove, OptionKeep)
	boom := errors.New("swap lane down")
	d2 := NewDecisions(v.e, AnswerHooks{ApproveSwap: func(context.Context, Decision, Item) error { return boom }})
	if _, err := d2.Answer(ctx, id2, OptionApprove, ""); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the hook's error", err)
	}
	if v.openDecisions(item2) != 1 {
		t.Error("a failed hook must leave the decision open")
	}
}

// An approved swap whose candidate expired from staging searches again instead.
func TestAnswerApproveForExpiredCandidateSearchesAgain(t *testing.T) {
	v := newEnv(t)
	called := false
	d := NewDecisions(v.e, AnswerHooks{ApproveSwap: func(context.Context, Decision, Item) error { called = true; return nil }})
	item := v.seedItem(KindUpgrade, StateSearching, v.seedRecording("r3", "S", "A", "", ""))
	cand := v.decCandidate(item, "", v.clock.Now().Add(-time.Hour).Unix())
	id := v.decWaiting(item, "different_master", cand, OptionApprove, OptionKeep)
	res, err := d.Answer(context.Background(), id, OptionApprove, "")
	if err != nil {
		t.Fatal(err)
	}
	if called || res.State != StateSearching {
		t.Fatalf("hook called=%v state=%s, want a new search and no hook", called, res.State)
	}
}

func TestAnswerRejectsBadOptionsAndSecondAnswers(t *testing.T) {
	v := newEnv(t)
	d := NewDecisions(v.e, AnswerHooks{})
	ctx := context.Background()
	item := v.seedItem(KindRequest, StateSearching, 0)
	id := v.decWaiting(item, "no_genuine_copy", 0, decNoGenuine...)

	if _, err := d.Answer(ctx, id, OptionKeep, ""); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("option not offered: err = %v", err)
	}
	if _, err := d.Answer(ctx, 9999, OptionDrop, ""); !errors.Is(err, ErrDecisionNotFound) {
		t.Fatalf("unknown decision: err = %v", err)
	}
	if v.openDecisions(item) != 1 || v.item(item).State != StateWaitingBlair {
		t.Fatal("a refused answer must change nothing")
	}
	if _, err := d.Answer(ctx, id, OptionDrop, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Answer(ctx, id, OptionDrop, ""); !errors.Is(err, ErrDecisionAnswered) {
		t.Fatalf("second answer: err = %v, want ErrDecisionAnswered", err)
	}
}
