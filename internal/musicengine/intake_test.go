package musicengine

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/tristenlammi/arrmada/internal/spotify"
)

func ninety() []spotify.Entry {
	var es []spotify.Entry
	for i := 0; i < 90; i++ {
		es = append(es, entry(fmt.Sprintf("sp%03d", i), fmt.Sprintf("Song %d", i), "Artist", fmt.Sprintf("ISRC%03d", i)))
	}
	return es
}

func TestFirstReadCreatesOneItemPerEntryAndSecondReadCreatesNone(t *testing.T) {
	v := newEnv(t)
	v.spotify.set(ninety()...)

	rep := v.sync()
	if rep.Created != 90 || v.count(`SELECT COUNT(*) FROM mus_items`) != 90 {
		t.Fatalf("first read: report %+v, items %d", rep, v.count(`SELECT COUNT(*) FROM mus_items`))
	}
	it := v.itemFor("sp000")
	if it.State != StateNew || it.Kind != KindRequest || it.Priority != PriorityIntake {
		t.Fatalf("new item = %+v", it)
	}

	rep = v.sync()
	if rep.Created != 0 || rep.Removed != 0 || v.count(`SELECT COUNT(*) FROM mus_items`) != 90 {
		t.Fatalf("second identical read: report %+v, items %d", rep, v.count(`SELECT COUNT(*) FROM mus_items`))
	}
	if v.count(`SELECT COUNT(*) FROM mus_playlists WHERE first_synced_at > 0 AND last_error = ''`) != 1 {
		t.Fatal("playlist row should record the sync")
	}
}

func TestSongAddedTwiceIsOneItemAndRemovingOneCopyKeepsIt(t *testing.T) {
	v := newEnv(t)
	a, b := entry("spA", "A", "X", "I1"), entry("spB", "B", "X", "I2")
	v.spotify.set(a, b, a) // A twice
	rep := v.sync()
	if rep.Created != 2 || v.count(`SELECT COUNT(*) FROM mus_items`) != 2 {
		t.Fatalf("duplicate entry made %d items (report %+v), want 2", v.count(`SELECT COUNT(*) FROM mus_items`), rep)
	}
	itemA := v.itemFor("spA")

	v.spotify.set(a, b) // one copy removed
	rep = v.sync()
	if rep.Removed != 0 || rep.Cancelled != 0 {
		t.Fatalf("one copy removed: report %+v", rep)
	}
	if got := v.item(itemA.ID).State; got != StateNew {
		t.Fatalf("item state = %s, must survive while a copy remains", got)
	}
}

func TestEntryMergedIntoAnotherItemIsNotCancelledWhileASiblingEntryRemains(t *testing.T) {
	v := newEnv(t)
	v.spotify.set(entry("sp1", "S", "X", "I1"), entry("sp2", "S (single)", "X", "I1"))
	v.sync()
	first := v.itemFor("sp1")
	// Pinning merged the second request into the first: its entry now points at the survivor.
	if _, err := v.db.Exec(`UPDATE mus_playlist_entries SET item_id = ? WHERE spotify_track_id = 'sp2'`, first.ID); err != nil {
		t.Fatal(err)
	}
	v.spotify.set(entry("sp1", "S", "X", "I1")) // sp2 removed, sp1 still present
	rep := v.sync()
	if rep.Removed != 1 || rep.Cancelled != 0 || rep.Kept != 1 {
		t.Fatalf("report %+v, want 1 removed / 0 cancelled / 1 kept", rep)
	}
	if got := v.item(first.ID).State; got != StateNew {
		t.Fatalf("surviving item state = %s", got)
	}
}

func TestRemovingASongWhileSearchingCancelsItAndWhileDownloadingKeepsIt(t *testing.T) {
	v := newEnv(t)
	v.spotify.set(entry("spS", "S", "X", "I1"), entry("spD", "D", "X", "I2"), entry("spK", "K", "X", "I3"))
	v.sync()
	searching, downloading := v.itemFor("spS"), v.itemFor("spD")
	if _, err := v.db.Exec(`UPDATE mus_items SET state='searching' WHERE id = ?`, searching.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := v.db.Exec(`UPDATE mus_items SET state='downloading' WHERE id = ?`, downloading.ID); err != nil {
		t.Fatal(err)
	}

	v.spotify.set(entry("spK", "K", "X", "I3")) // S and D removed
	rep := v.sync()
	if rep.Removed != 2 || rep.Cancelled != 1 || rep.Kept != 1 {
		t.Fatalf("report %+v, want 2 removed / 1 cancelled / 1 kept", rep)
	}
	if got := v.item(searching.ID); got.State != StateCancelled || got.LastReason == "" {
		t.Fatalf("searching item = %s %q, want cancelled with a reason (AE11)", got.State, got.LastReason)
	}
	if got := v.item(downloading.ID).State; got != StateDownloading {
		t.Fatalf("downloading item = %s, must keep running (AE11)", got)
	}
	// The entry rows stay, marked absent, so a re-add can find the item.
	if v.count(`SELECT COUNT(*) FROM mus_playlist_entries WHERE present = 0`) != 2 {
		t.Fatal("removed entries should stay as absent rows")
	}
}

func TestReAddingACancelledSongReopensTheSameItem(t *testing.T) {
	v := newEnv(t)
	v.spotify.set(entry("spA", "A", "X", "I1"), entry("spB", "B", "X", "I2"))
	v.sync()
	orig := v.itemFor("spA")

	v.spotify.set(entry("spB", "B", "X", "I2"))
	v.sync()
	if v.item(orig.ID).State != StateCancelled {
		t.Fatal("setup: item should be cancelled")
	}

	v.spotify.set(entry("spA", "A", "X", "I1"), entry("spB", "B", "X", "I2"))
	rep := v.sync()
	if rep.Reopened != 1 || rep.Created != 0 {
		t.Fatalf("report %+v, want 1 reopened and 0 created", rep)
	}
	if got := v.itemFor("spA"); got.ID != orig.ID || got.State != StateNew {
		t.Fatalf("item = %d %s, want the same item %d reopened to new", got.ID, got.State, orig.ID)
	}
	if v.count(`SELECT COUNT(*) FROM mus_items`) != 2 {
		t.Fatal("re-add created a second item")
	}
}

func TestReAddingADoneSongLeavesItDone(t *testing.T) {
	v := newEnv(t)
	v.spotify.set(entry("spA", "A", "X", "I1"), entry("spB", "B", "X", "I2"))
	v.sync()
	a := v.itemFor("spA")
	if _, err := v.db.Exec(`UPDATE mus_items SET state='done', deadline_at=0 WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	v.spotify.set(entry("spB", "B", "X", "I2"))
	v.sync()
	v.spotify.set(entry("spA", "A", "X", "I1"), entry("spB", "B", "X", "I2"))
	v.sync()
	if got := v.item(a.ID).State; got != StateDone {
		t.Fatalf("done item state = %s after remove + re-add, want done", got)
	}
}

func TestFailedReadChangesNothingAndRaisesOneAlert(t *testing.T) {
	v := newEnv(t)
	v.spotify.set(ninety()[:5]...)
	v.sync()
	before := v.count(`SELECT COUNT(*) FROM mus_items WHERE state = 'new'`)

	v.spotify.mu.Lock()
	v.spotify.err = fmt.Errorf("%w: page 2 returned 500", spotify.ErrIncomplete)
	v.spotify.mu.Unlock()
	for i := 0; i < 3; i++ {
		_, err := v.e.SyncIntake(context.Background())
		if !errors.Is(err, spotify.ErrIncomplete) {
			t.Fatalf("read %d err = %v, want it to wrap the read failure", i, err)
		}
	}
	if got := v.count(`SELECT COUNT(*) FROM mus_items WHERE state = 'new'`); got != before {
		t.Fatalf("items changed on a failed read: %d -> %d (AE12)", before, got)
	}
	if v.count(`SELECT COUNT(*) FROM mus_playlist_entries WHERE present = 1`) != 5 {
		t.Fatal("entries changed on a failed read")
	}
	if n := v.notifier.count(AlertPlaylistReadFailed); n != 1 {
		t.Fatalf("alerts = %d over a 3-read failure streak, want 1", n)
	}
	var lastErr string
	_ = v.db.QueryRow(`SELECT last_error FROM mus_playlists`).Scan(&lastErr)
	if lastErr == "" {
		t.Fatal("the failure reason was not kept on the playlist row")
	}

	// Recovery clears the error, and a later failure alerts again.
	v.spotify.set(ninety()[:5]...)
	v.sync()
	v.spotify.mu.Lock()
	v.spotify.err = errors.New("403")
	v.spotify.mu.Unlock()
	_, _ = v.e.SyncIntake(context.Background())
	if n := v.notifier.count(AlertPlaylistReadFailed); n != 2 {
		t.Fatalf("alerts after a second streak = %d, want 2", n)
	}
}

func TestEmptyReadOfAPlaylistThatHeldSongsIsTreatedAsAFailedRead(t *testing.T) {
	v := newEnv(t)
	v.spotify.set(ninety()[:3]...)
	v.sync()
	v.spotify.set() // no error, no entries
	if _, err := v.e.SyncIntake(context.Background()); !errors.Is(err, errEmptyRead) {
		t.Fatalf("err = %v, want errEmptyRead", err)
	}
	if v.count(`SELECT COUNT(*) FROM mus_playlist_entries WHERE present = 1`) != 3 {
		t.Fatal("an empty read removed entries")
	}
	if v.notifier.count(AlertPlaylistReadFailed) != 1 {
		t.Fatal("an empty read of a non-empty playlist must alert")
	}
}

func TestAlertRefusedIsRetriedOnTheNextFailedRead(t *testing.T) {
	v := newEnv(t)
	v.spotify.mu.Lock()
	v.spotify.err = errors.New("401")
	v.spotify.mu.Unlock()
	v.notifier.err = errors.New("queue down")
	_, _ = v.e.SyncIntake(context.Background())
	v.notifier.err = nil
	_, _ = v.e.SyncIntake(context.Background())
	if n := v.notifier.count(AlertPlaylistReadFailed); n != 1 {
		t.Fatalf("alerts = %d, want 1 once the notifier accepted one", n)
	}
	_, _ = v.e.SyncIntake(context.Background())
	if n := v.notifier.count(AlertPlaylistReadFailed); n != 1 {
		t.Fatalf("alerts = %d after a third failure, want still 1", n)
	}
}

func TestLocalEntryGetsAStableKeyAndOneItem(t *testing.T) {
	v := newEnv(t)
	local := spotify.Entry{Type: "track", IsLocal: true, Name: "Demo tape", Artists: []string{"Me"}, DurationMs: 1000}
	v.spotify.set(local)
	if rep := v.sync(); rep.Created != 1 {
		t.Fatalf("report %+v", rep)
	}
	if rep := v.sync(); rep.Created != 0 {
		t.Fatalf("local entry re-created: %+v", rep)
	}
	if v.count(`SELECT COUNT(*) FROM mus_playlist_entries WHERE is_local = 1`) != 1 {
		t.Fatal("local entry not flagged")
	}
}

func TestNoPlaylistConfiguredDoesNothing(t *testing.T) {
	v := newEnv(t)
	v.settings.PlaylistID = ""
	rep, err := v.e.SyncIntake(context.Background())
	if err != nil || rep != (SyncReport{}) || v.spotify.calls != 0 {
		t.Fatalf("rep %+v err %v calls %d", rep, err, v.spotify.calls)
	}
}
