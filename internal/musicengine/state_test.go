package musicengine

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tristenlammi/arrmada/internal/store"
)

func TestTransitionWritesStateEnteredDeadlineAndReasonTogether(t *testing.T) {
	v := newEnv(t)
	id := v.seedItem(KindRequest, StateNew, 0)
	v.clock.Advance(time.Minute)

	if err := v.e.Transition(context.Background(), id, StatePinning, "starting"); err != nil {
		t.Fatal(err)
	}
	it := v.item(id)
	now := v.clock.Now()
	if it.State != StatePinning || it.EnteredAt != now.Unix() || it.LastReason != "starting" {
		t.Fatalf("state/entered/reason = %s/%d/%q", it.State, it.EnteredAt, it.LastReason)
	}
	if want := now.Add(DeadlinePinning).Unix(); it.DeadlineAt != want {
		t.Fatalf("deadline = %d, want %d (pinning default)", it.DeadlineAt, want)
	}
}

func TestEveryStateHasItsDeadlineAndSuspendingStatesHaveNone(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	for _, s := range []State{StateNew, StatePinning, StateLibraryCheck, StateSearching, StateLaneUnavailable,
		StateDownloading, StateChecking, StateFailedCheck, StatePlacing, StateSwapping,
		StateWaitingForPlayback, StateAwaitingPlexIndex} {
		if DeadlineFor(s, now) <= now.Unix() {
			t.Errorf("%s has no deadline", s)
		}
	}
	for _, s := range []State{StatePaused, StateWaitingBlair, StateDone, StateCancelled, StateDropped} {
		if d := DeadlineFor(s, now); d != 0 {
			t.Errorf("%s deadline = %d, want none (KTD4)", s, d)
		}
	}
	if DeadlinePinning != time.Hour || DeadlineLibraryCheck != 2*time.Hour || DeadlineSearching != 24*time.Hour ||
		DeadlineDownloading != 6*time.Hour || DeadlineChecking != time.Hour || DeadlinePlacing != time.Hour ||
		DeadlineAwaitingPlexIndex != time.Hour {
		t.Error("a documented default deadline changed")
	}
}

func TestInvalidTransitionIsRefusedAndChangesNothing(t *testing.T) {
	v := newEnv(t)
	id := v.seedItem(KindRequest, StateNew, 0)
	err := v.e.Transition(context.Background(), id, StateDownloading, "skip ahead")
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
	if v.item(id).State != StateNew {
		t.Fatal("state changed on a refused transition")
	}
	if err := v.e.Transition(context.Background(), 9999, StatePinning, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown item err = %v, want ErrNotFound", err)
	}
}

func TestEveryDiagramEdgeIsAllowed(t *testing.T) {
	edges := [][2]State{
		{StateNew, StatePinning}, {StatePinning, StateLibraryCheck}, {StatePinning, StateWaitingBlair},
		{StateLibraryCheck, StateDone}, {StateLibraryCheck, StateSearching}, {StateLibraryCheck, StateWaitingBlair},
		{StateSearching, StateLaneUnavailable}, {StateLaneUnavailable, StateSearching},
		{StateSearching, StateDownloading}, {StateSearching, StateWaitingBlair},
		{StateDownloading, StateChecking}, {StateChecking, StateFailedCheck}, {StateFailedCheck, StateSearching},
		{StateChecking, StatePlacing}, {StateChecking, StateSwapping}, {StateChecking, StateWaitingBlair},
		{StateSwapping, StateWaitingForPlayback}, {StateWaitingForPlayback, StateSwapping},
		{StatePlacing, StateAwaitingPlexIndex}, {StateSwapping, StateAwaitingPlexIndex},
		{StateAwaitingPlexIndex, StateDone},
		{StateWaitingBlair, StateSearching}, {StateWaitingBlair, StatePlacing}, {StateWaitingBlair, StateSwapping},
		{StateWaitingBlair, StateDropped},
	}
	for _, ed := range edges {
		if !allowed(ed[0], ed[1], "") {
			t.Errorf("%s -> %s should be allowed (state diagram)", ed[0], ed[1])
		}
	}
	for _, s := range []State{StateDone, StateCancelled, StateDropped} {
		if allowed(s, StatePaused, "") || allowed(s, StatePinning, "") {
			t.Errorf("terminal %s must not move", s)
		}
	}
}

func TestPausedSuspendsDeadlineAndResumeReturnsToTheLeftState(t *testing.T) {
	v := newEnv(t)
	id := v.seedItem(KindRequest, StateSearching, 0)
	ctx := context.Background()

	if err := v.e.Pause(ctx, id, "low space"); err != nil {
		t.Fatal(err)
	}
	it := v.item(id)
	if it.State != StatePaused || it.DeadlineAt != 0 || it.PausedFromState != string(StateSearching) {
		t.Fatalf("paused item = %+v", it)
	}
	if err := v.e.Transition(ctx, id, StateDownloading, "no"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("paused item may only return to its own state, got %v", err)
	}

	v.clock.Advance(48 * time.Hour)
	if err := v.e.Resume(ctx, id, "space back"); err != nil {
		t.Fatal(err)
	}
	it = v.item(id)
	if it.State != StateSearching || it.PausedFromState != "" {
		t.Fatalf("resumed = %+v", it)
	}
	if want := v.clock.Now().Add(DeadlineSearching).Unix(); it.DeadlineAt != want {
		t.Fatalf("resumed deadline = %d, want fresh %d", it.DeadlineAt, want)
	}
}

func TestWaitingBlairHasNoDeadline(t *testing.T) {
	v := newEnv(t)
	id := v.seedItem(KindRequest, StatePinning, 0)
	if err := v.e.Transition(context.Background(), id, StateWaitingBlair, "ambiguous"); err != nil {
		t.Fatal(err)
	}
	if got := v.item(id).DeadlineAt; got != 0 {
		t.Fatalf("waiting_blair deadline = %d, want 0", got)
	}
}

func TestCancelOnlyBeforeDownloadStarts(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()

	early := v.seedItem(KindRequest, StateSearching, 0)
	if err := v.e.Transition(ctx, early, StateCancelled, "removed"); err != nil {
		t.Fatalf("cancel while searching: %v", err)
	}

	late := v.seedItem(KindRequest, StateDownloading, 0)
	if err := v.e.Transition(ctx, late, StateCancelled, "removed"); !errors.Is(err, ErrInvalidTransition) && !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("cancel while downloading err = %v", err)
	}

	// Back in searching after a candidate was downloaded and rejected: the download did start.
	retry := v.seedItem(KindRequest, StateSearching, 0)
	if _, err := v.db.Exec(`INSERT INTO mus_candidates (item_id, lane, state, created_at) VALUES (?, 'usenet', 'rejected', 1)`, retry); err != nil {
		t.Fatal(err)
	}
	if err := v.e.Transition(ctx, retry, StateCancelled, "removed"); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("cancel after a candidate was tried err = %v, want ErrNotCancellable", err)
	}
	if v.item(retry).State != StateSearching {
		t.Fatal("state changed on a refused cancel")
	}
}

func TestPausedItemCancelFollowsItsPreviousState(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()
	id := v.seedItem(KindRequest, StateSearching, 0)
	if err := v.e.Pause(ctx, id, "space"); err != nil {
		t.Fatal(err)
	}
	if err := v.e.Transition(ctx, id, StateCancelled, "removed"); err != nil {
		t.Fatalf("cancel of an item paused from searching: %v", err)
	}
	dl := v.seedItem(KindRequest, StateDownloading, 0)
	if err := v.e.Pause(ctx, dl, "space"); err != nil {
		t.Fatal(err)
	}
	if err := v.e.Transition(ctx, dl, StateCancelled, "removed"); err == nil {
		t.Fatal("an item paused from downloading must not cancel")
	}
}

func TestTransitionInsideRolledBackTxChangesNothing(t *testing.T) {
	v := newEnv(t)
	id := v.seedItem(KindRequest, StateNew, 0)
	boom := errors.New("boom")
	err := store.WithTx(context.Background(), v.db, func(tx *sql.Tx) error {
		if err := v.e.TransitionTx(context.Background(), tx, id, StatePinning, "x"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if v.item(id).State != StateNew {
		t.Fatal("the transition outlived its rolled-back transaction")
	}
}

func TestPastDeadlineRaisesOneAlertAndPausedRaisesNone(t *testing.T) {
	v := newEnv(t)
	late := v.seedItem(KindRequest, StateSearching, 0)
	paused := v.seedItem(KindRequest, StateSearching, 0)
	if err := v.e.Pause(context.Background(), paused, "space"); err != nil {
		t.Fatal(err)
	}

	v.clock.Advance(DeadlineSearching + time.Minute)
	for i := 0; i < 3; i++ {
		if err := v.e.CheckDeadlines(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := v.notifier.count(AlertItemPastDeadline); n != 1 {
		t.Fatalf("alerts = %d, want exactly 1 (the paused item raises none)", n)
	}
	if a := v.notifier.alerts[0]; a.ItemID != late {
		t.Fatalf("alert for item %d, want %d", a.ItemID, late)
	}

	// The same item in a new state is a new condition.
	if err := v.e.Transition(context.Background(), late, StateDownloading, "found"); err != nil {
		t.Fatal(err)
	}
	v.clock.Advance(DeadlineDownloading + time.Minute)
	_ = v.e.CheckDeadlines(context.Background())
	if n := v.notifier.count(AlertItemPastDeadline); n != 2 {
		t.Fatalf("alerts after a new overdue state = %d, want 2", n)
	}
}

func TestRefusedDeadlineAlertIsRetriedNextRun(t *testing.T) {
	v := newEnv(t)
	v.seedItem(KindRequest, StateNew, 0)
	v.clock.Advance(DeadlineNew + time.Minute)
	v.notifier.err = errors.New("queue down")
	if err := v.e.CheckDeadlines(context.Background()); err == nil {
		t.Fatal("a refused alert must surface as an error")
	}
	v.notifier.err = nil
	if err := v.e.CheckDeadlines(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := v.notifier.count(AlertItemPastDeadline); n != 1 {
		t.Fatalf("alerts = %d, want 1 after the retry", n)
	}
}

func TestTickAdvancesNewItemsAndSeamsRunWithDefaults(t *testing.T) {
	v := newEnv(t)
	id := v.seedItem(KindRequest, StateNew, 0)
	v.tick(1)
	if v.item(id).State != StatePinning {
		t.Fatalf("state = %s, want pinning", v.item(id).State)
	}
	// With every seam at its default, an item in searching just waits: no panic, no change.
	bare, err := New(Config{DB: v.db, Now: v.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	s := v.seedItem(KindRequest, StateSearching, 0)
	if err := bare.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v.item(s).State != StateSearching {
		t.Fatal("the no-op acquirer must leave the item where it is")
	}
}

func TestStepErrorKeepsStateRecordsReasonAndBacksOff(t *testing.T) {
	v := newEnv(t)
	rec := v.seedRecording("rec-1", "Song", "Artist", "ISRC1", "studio")
	_ = rec
	id := v.seedItem(KindRequest, StatePinning, 0)
	v.mb.err = errors.New("503 service unavailable")
	if _, err := v.db.Exec(`INSERT INTO mus_playlists (id, spotify_id, created_at) VALUES (1, 'pl1', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := v.db.Exec(`INSERT INTO mus_playlist_entries (playlist_id, spotify_track_id, isrc, title, artist, item_id, first_seen_at)
		VALUES (1, 'sp1', 'ISRC1', 'Song', 'Artist', ?, 1)`, id); err != nil {
		t.Fatal(err)
	}

	v.tick(1)
	it := v.item(id)
	if it.State != StatePinning || it.Attempts != 1 || it.NextAttemptAt <= v.clock.Now().Unix() {
		t.Fatalf("after a failed step: %+v", it)
	}
	if it.LastReason == "" || !strings.Contains(it.LastReason, "503") {
		t.Fatalf("last reason %q lost the cause", it.LastReason)
	}

	// Not due yet: a second tick must not call MusicBrainz again.
	calls := len(v.mb.classes)
	v.tick(1)
	if len(v.mb.classes) != calls {
		t.Fatal("item retried before its backoff elapsed")
	}

	// A restart (new engine, same DB) picks it up in the same state once due.
	v.mb.err = nil
	v.mb.add(mbRec("rec-1", "Song", "Artist", "", "ISRC1"))
	v.clock.Advance(time.Hour)
	v.e = v.newEngine()
	v.tick(1)
	if got := v.item(id).State; got != StateLibraryCheck {
		t.Fatalf("after restart state = %s, want library_check", got)
	}
}

func TestPausedItemsResumeWhenTheGateReopens(t *testing.T) {
	v := newEnv(t)
	gate := &fakeGate{ok: false, reason: "below floor"}
	e, err := New(Config{DB: v.db, Now: v.clock.Now, Space: gate, Acquirer: v.acquirer})
	if err != nil {
		t.Fatal(err)
	}
	id := v.seedItem(KindRequest, StateSearching, 0)
	if err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v.item(id).State != StatePaused {
		t.Fatalf("state = %s, want paused by the closed gate", v.item(id).State)
	}
	if len(v.acquirer.calls) != 0 {
		t.Fatal("acquirer ran while the gate was closed")
	}
	gate.ok = true
	if err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := v.item(id).State; got != StateSearching {
		t.Fatalf("state = %s, want searching after the gate reopened", got)
	}
}
