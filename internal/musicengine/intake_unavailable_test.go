package musicengine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tristenlammi/arrmada/internal/spotify"
)

// Astra #12: a null slot has no identity, so a known entry missing from the read is not
// proven removed. Nothing is removed or cancelled that run.
func TestNullSlotInThePlaylistRemovesAndCancelsNothing(t *testing.T) {
	v := newEnv(t)
	a, b := entry("spA", "A", "X", "I1"), entry("spB", "B", "X", "I2")
	v.spotify.set(a, b)
	v.sync()

	v.spotify.set(a, spotify.Entry{Position: 1, Unavailable: true}) // B's slot now reads as a null item
	rep := v.sync()
	if rep.Removed != 0 || rep.Cancelled != 0 || rep.Skipped != 1 {
		t.Fatalf("report %+v, want nothing removed or cancelled and 1 skipped slot", rep)
	}
	if n := v.count(`SELECT COUNT(*) FROM mus_playlist_entries WHERE present = 1`); n != 2 {
		t.Fatalf("present entries = %d, want 2", n)
	}
	if got := v.itemFor("spB").State; got != StateNew {
		t.Fatalf("item B state = %s, want it left running", got)
	}

	// A complete read with no null slot then removes it as before.
	v.spotify.set(a)
	if rep := v.sync(); rep.Removed != 1 || rep.Cancelled != 1 {
		t.Fatalf("clean read report %+v, want B removed and cancelled", rep)
	}
}

// An identified track Spotify marks unavailable keeps that flag and is routed like a local
// entry (to Blair, pin.go), not searched; it is not marked local.
func TestUnavailableIdentifiedEntryKeepsItsFlag(t *testing.T) {
	v := newEnv(t)
	u := entry("spU", "U", "X", "I9")
	u.Unavailable = true
	v.spotify.set(entry("spA", "A", "X", "I1"), u)
	v.sync()
	if n := v.count(`SELECT COUNT(*) FROM mus_playlist_entries WHERE spotify_track_id = 'spU' AND unavailable = 1 AND is_local = 0`); n != 1 {
		t.Fatalf("unavailable entry not flagged")
	}
	if n := v.count(`SELECT COUNT(*) FROM mus_playlist_entries WHERE spotify_track_id = 'spA' AND unavailable = 0 AND is_local = 0`); n != 1 {
		t.Fatalf("available entry wrongly flagged")
	}
	// It comes back playable: the flag clears.
	v.spotify.set(entry("spA", "A", "X", "I1"), entry("spU", "U", "X", "I9"))
	v.sync()
	if n := v.count(`SELECT COUNT(*) FROM mus_playlist_entries WHERE spotify_track_id = 'spU' AND unavailable = 0`); n != 1 {
		t.Fatal("flag did not clear when the track became playable")
	}
}

// Astra #22: a saved Spotify cool-down skips the run without an error, an alert or a recorded
// read failure.
func TestSpotifyBackoffSkipsIntakeQuietly(t *testing.T) {
	v := newEnv(t)
	v.spotify.set(entry("spA", "A", "X", "I1"))
	v.sync()
	v.spotify.mu.Lock()
	v.spotify.err = fmt.Errorf("reading playlist: %w", &spotify.BackoffError{Until: v.clock.Now().Add(time.Hour)})
	v.spotify.mu.Unlock()
	rep, err := v.e.SyncIntake(context.Background())
	if err != nil || rep.Entries != 0 {
		t.Fatalf("SyncIntake = %+v, %v; want a quiet skip", rep, err)
	}
	if n := v.notifier.count(AlertPlaylistReadFailed); n != 0 {
		t.Fatalf("alerts = %d, want none for a cool-down", n)
	}
	var lastErr string
	_ = v.db.QueryRow(`SELECT last_error FROM mus_playlists`).Scan(&lastErr)
	if lastErr != "" {
		t.Fatalf("last_error = %q, a cool-down is not a failure", lastErr)
	}
	if n := v.count(`SELECT COUNT(*) FROM mus_playlist_entries WHERE present = 1`); n != 1 {
		t.Fatal("entries changed during a cool-down")
	}
}
