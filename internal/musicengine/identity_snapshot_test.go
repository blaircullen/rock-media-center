package musicengine

import (
	"context"
	"testing"
)

// Astra #14: with the rating key unchanged nothing was replaced, so a rating change or a
// playlist removal made in Plex since the snapshot must survive the nightly remap.
func TestRemapKeepsLaterPlexChangesWhenTheKeyIsUnchanged(t *testing.T) {
	f := newMirrorFix(t)
	ctx := context.Background()
	keyA := f.keyOf(f.itemA)
	road, _ := f.plex.CreatePlaylist(ctx, "Road Trip", []string{keyA})
	_ = f.plex.SetRating(ctx, keyA, 8)
	f.recordState(f.itemA, 8, road, "Road Trip")

	// The user changes their mind in Plex: new rating, track out of the playlist.
	_ = f.plex.SetRating(ctx, keyA, 3)
	f.plex.playlists[road].Keys = nil

	rep, err := f.remapper(false).Remap(ctx)
	if err != nil {
		t.Fatalf("remap: %v (%+v)", err, rep)
	}
	if got := f.plex.ratings[keyA]; got != 3 {
		t.Errorf("rating in Plex = %v, want the user's 3 left alone", got)
	}
	if len(f.plex.playlists[road].Keys) != 0 {
		t.Errorf("Road Trip = %v, want the track left out", f.plex.playlists[road].Keys)
	}
	var rating float64
	if err := f.v.db.QueryRow(`SELECT rating FROM mus_track_identities WHERE recording_id = ?`, f.v.item(f.itemA).RecordingID).Scan(&rating); err != nil || rating != 3 {
		t.Errorf("recorded rating = %v (%v), want the snapshot refreshed to 3", rating, err)
	}
	if n := f.v.count(`SELECT COUNT(*) FROM mus_track_memberships WHERE is_mirror = 0`); n != 0 {
		t.Errorf("memberships = %d, want the removed one dropped from the snapshot", n)
	}

	// And a new playlist the user added the track to is learned, not accumulated twice.
	other, _ := f.plex.CreatePlaylist(ctx, "Gym", []string{keyA})
	for i := 0; i < 2; i++ {
		if _, err := f.remapper(false).Remap(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.v.count(`SELECT COUNT(*) FROM mus_track_memberships WHERE is_mirror = 0 AND plex_playlist_key = ?`, other); n != 1 {
		t.Errorf("memberships on Gym = %d, want exactly 1", n)
	}
}

// Astra #10: the catalogue refresh runs before a remap and inserts the re-imported key as a
// second row at the same path; the remap must merge the two, not fail the unique key.
func TestRemapMergesTheCatalogueRowInsertedForTheNewKey(t *testing.T) {
	f := newMirrorFix(t)
	ctx := context.Background()
	f.recordState(f.itemA, 0, "", "")
	trackID := f.v.item(f.itemA).TrackID
	var path string
	if err := f.v.db.QueryRow(`SELECT path FROM mus_library_tracks WHERE id = ?`, trackID).Scan(&path); err != nil {
		t.Fatal(err)
	}
	f.plex.reimport()
	newKey := f.plex.keyFor(f.plex.toPlex(mustPath(t, f, f.itemA)))
	if _, err := f.v.db.Exec(
		`INSERT INTO mus_library_tracks (plex_rating_key, path, title, user_rating, play_count, created_at) VALUES (?, ?, 'Song A', 6, 4, 1)`,
		newKey, path); err != nil {
		t.Fatal(err)
	}

	rep, err := f.remapper(false).Remap(ctx)
	if err != nil {
		t.Fatalf("remap: %v (%+v)", err, rep)
	}
	if n := f.v.count(`SELECT COUNT(*) FROM mus_library_tracks WHERE path = ?`, path); n != 1 {
		t.Fatalf("library rows at the path = %d, want 1 merged", n)
	}
	var key string
	var rating float64
	var plays int
	if err := f.v.db.QueryRow(`SELECT plex_rating_key, COALESCE(user_rating,0), play_count FROM mus_library_tracks WHERE id = ?`, trackID).Scan(&key, &rating, &plays); err != nil {
		t.Fatalf("the original library row is gone: %v", err)
	}
	if key != newKey || rating != 6 || plays != 4 {
		t.Errorf("merged row = key %q rating %v plays %d, want %q 6 4", key, rating, plays, newKey)
	}
	if got := f.v.item(f.itemA).TrackID; got != trackID {
		t.Errorf("item now points at library row %d, want %d", got, trackID)
	}
}
