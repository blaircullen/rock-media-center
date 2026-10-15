package musicengine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// mirrorFix is a rig with two placed songs and a watched playlist holding both.
type mirrorFix struct {
	*placeRig
	mirror         *Mirror
	itemA, itemB   int64
	entryA, entryB int64
	plID           int64
}

func (r *placeRig) placeSong(title string, track int) int64 {
	r.t.Helper()
	s := r.seed(placeSeed{Title: title, Artist: "Band", Album: "Album", Disc: 1, Track: track})
	r.drive(s.ItemID, 5)
	return s.ItemID
}

func (r *placeRig) addEntry(plID int64, spotifyID string, itemID int64, firstSeen int64) int64 {
	r.t.Helper()
	it := r.v.item(itemID)
	res, err := r.v.db.Exec(
		`INSERT INTO mus_playlist_entries (playlist_id, spotify_track_id, title, recording_id, item_id, present, first_seen_at)
		 VALUES (?, ?, ?, ?, ?, 1, ?)`, plID, spotifyID, spotifyID, nullID(it.RecordingID), itemID, firstSeen)
	if err != nil {
		r.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func newMirrorFix(t *testing.T) *mirrorFix {
	r := newPlaceRig(t)
	f := &mirrorFix{placeRig: r}
	f.itemA, f.itemB = r.placeSong("Song A", 1), r.placeSong("Song B", 2)
	res, err := r.v.db.Exec(`INSERT INTO mus_playlists (spotify_id, name, created_at) VALUES ('pl1', 'My Shazam Tracks', ?)`, r.v.clock.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	f.plID, _ = res.LastInsertId()
	f.entryA = r.addEntry(f.plID, "sp-a", f.itemA, 100)
	f.entryB = r.addEntry(f.plID, "sp-b", f.itemB, 200)
	f.mirror, err = NewMirror(MirrorConfig{DB: r.v.db, Plex: r.plex, PlaylistID: func() string { return "pl1" }, Now: r.v.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *mirrorFix) keyOf(itemID int64) string {
	var k string
	if err := f.v.db.QueryRow(`SELECT lt.plex_rating_key FROM mus_items i JOIN mus_library_tracks lt ON lt.id = i.track_id WHERE i.id = ?`, itemID).Scan(&k); err != nil {
		f.t.Fatal(err)
	}
	return k
}

func (f *mirrorFix) mirrorKeys() []string {
	f.t.Helper()
	for _, p := range f.plex.playlists {
		if p.Title == "My Shazam Tracks (mirror)" {
			ks := append([]string(nil), p.Keys...)
			sort.Strings(ks)
			return ks
		}
	}
	return nil
}

func sortedKeys(ks ...string) []string { sort.Strings(ks); return ks }

func TestMirrorHoldsPresentLibrarySongsAndRecordsMemberships(t *testing.T) {
	f := newMirrorFix(t)
	// A song on the playlist with no library file yet is not a member.
	f.addEntry(f.plID, "sp-pending", f.v.seedItem(KindRequest, StateSearching, 0), 300)

	rep, err := f.mirror.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Created || rep.Members != 2 || rep.Title != "My Shazam Tracks (mirror)" {
		t.Errorf("report = %+v, want a created 2-member playlist with the mirror title", rep)
	}
	if got, want := f.mirrorKeys(), sortedKeys(f.keyOf(f.itemA), f.keyOf(f.itemB)); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Plex mirror = %v, want %v", got, want)
	}
	if n := f.v.count(`SELECT COUNT(*) FROM mus_track_memberships WHERE is_mirror = 1`); n != 2 {
		t.Errorf("mirror memberships recorded = %d, want 2", n)
	}
	// A second sync with nothing to do writes nothing.
	rep, err = f.mirror.Sync(context.Background())
	if err != nil || rep.Added != 0 || rep.Removed != 0 || rep.Created {
		t.Errorf("idempotent sync: %+v %v", rep, err)
	}
}

func TestMirrorDropsSongRemovedFromSpotifyButKeepsTheFile(t *testing.T) {
	f := newMirrorFix(t)
	if _, err := f.mirror.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	var path string
	_ = f.v.db.QueryRow(`SELECT new_path FROM mus_swap_journal WHERE item_id = ?`, f.itemA).Scan(&path)
	if _, err := f.v.db.Exec(`UPDATE mus_playlist_entries SET present = 0, removed_at = 5 WHERE id = ?`, f.entryA); err != nil {
		t.Fatal(err)
	}
	rep, err := f.mirror.Sync(context.Background())
	if err != nil || rep.Removed != 1 {
		t.Fatalf("sync = %+v %v, want one removal", rep, err)
	}
	if got := f.mirrorKeys(); fmt.Sprint(got) != fmt.Sprint([]string{f.keyOf(f.itemB)}) {
		t.Errorf("mirror = %v, want only song B", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the library file for the removed song is gone: %v", err)
	}
	if n := f.v.count(`SELECT COUNT(*) FROM mus_track_memberships WHERE is_mirror = 1`); n != 1 {
		t.Errorf("mirror memberships = %d, want 1", n)
	}
	if it := f.v.item(f.itemA); it.State != StateDone {
		t.Errorf("item A = %s, want still done", it.State)
	}
}

func TestMirrorAddsASongPlacedLater(t *testing.T) {
	f := newMirrorFix(t)
	if _, err := f.mirror.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	itemC := f.placeSong("Song C", 3)
	f.addEntry(f.plID, "sp-c", itemC, 400)
	rep, err := f.mirror.Sync(context.Background())
	if err != nil || rep.Added != 1 || rep.Members != 3 {
		t.Errorf("sync = %+v %v, want one addition, 3 members", rep, err)
	}
}

func TestMirrorReadBackCatchesAWriteThatDidNotLand(t *testing.T) {
	f := newMirrorFix(t)
	if _, err := f.mirror.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	itemC := f.placeSong("Song C", 3)
	f.addEntry(f.plID, "sp-c", itemC, 400)
	f.plex.dropAdds = true // Plex says yes and does nothing
	_, err := f.mirror.Sync(context.Background())
	if err == nil || !strings.Contains(err.Error(), "reads back") {
		t.Fatalf("err = %v, want a read-back mismatch", err)
	}
}

func TestMirrorWithTwoSameTitledPlaylistsChangesNothing(t *testing.T) {
	f := newMirrorFix(t)
	ctx := context.Background()
	_, _ = f.plex.CreatePlaylist(ctx, "My Shazam Tracks (mirror)", []string{"x"})
	_, _ = f.plex.CreatePlaylist(ctx, "My Shazam Tracks (mirror)", []string{"y"})
	_, err := f.mirror.Sync(ctx)
	if err == nil || !strings.Contains(err.Error(), "2 playlists named") {
		t.Fatalf("err = %v, want an ambiguity error", err)
	}
	for _, p := range f.plex.playlists {
		if len(p.Keys) != 1 {
			t.Errorf("playlist %q was written to: %v", p.Title, p.Keys)
		}
	}
}

func TestMirrorWithNoMembersCreatesNothing(t *testing.T) {
	f := newMirrorFix(t)
	if _, err := f.v.db.Exec(`UPDATE mus_playlist_entries SET present = 0`); err != nil {
		t.Fatal(err)
	}
	rep, err := f.mirror.Sync(context.Background())
	if err != nil || rep.Created || len(f.plex.playlists) != 0 {
		t.Errorf("sync = %+v %v playlists %d, want nothing created", rep, err, len(f.plex.playlists))
	}
}

// ---- remap after a Plex re-import (R19) ----

func (f *mirrorFix) remapper(withMirror bool) *Remapper {
	var m *Mirror
	if withMirror {
		m = f.mirror
	}
	rm, err := NewRemapper(RemapConfig{
		DB: f.v.db, Plex: f.plex, PlexSection: func() string { return "5" },
		LibraryRoot: f.lib, PlexRoot: f.plexRt, Mirror: m, Now: f.v.clock.Now,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return rm
}

// recordState records a rating and a non-mirror playlist membership for a song, the way U11
// does before a swap.
func (f *mirrorFix) recordState(itemID int64, rating float64, playlistKey, title string) {
	f.t.Helper()
	it := f.v.item(itemID)
	var path, sha, key string
	if err := f.v.db.QueryRow(`SELECT path, sha256, plex_rating_key FROM mus_library_tracks WHERE id = ?`, it.TrackID).Scan(&path, &sha, &key); err != nil {
		f.t.Fatal(err)
	}
	id, err := RecordIdentity(context.Background(), f.v.db, IdentityRecord{RecordingID: it.RecordingID, Path: path, SHA256: sha, RatingKey: key, Rating: &rating}, f.v.clock.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	if playlistKey != "" {
		if err := RecordMembership(context.Background(), f.v.db, id, playlistKey, title, false, f.v.clock.Now()); err != nil {
			f.t.Fatal(err)
		}
	}
}

func TestRemapRestoresMirrorMembershipAndRatingAfterReimport(t *testing.T) {
	f := newMirrorFix(t)
	ctx := context.Background()
	keyA, keyB := f.keyOf(f.itemA), f.keyOf(f.itemB)
	if _, err := f.mirror.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	road, _ := f.plex.CreatePlaylist(ctx, "Road Trip", []string{keyA})
	_ = f.plex.SetRating(ctx, keyA, 8)
	f.recordState(f.itemA, 8, road, "Road Trip")
	f.recordState(f.itemB, 0, "", "")
	_ = keyB

	f.plex.reimport("Road Trip")
	newA := f.plex.keyFor(f.plex.toPlex(mustPath(t, f, f.itemA)))
	if newA == keyA {
		t.Fatal("test setup: re-import did not change the key")
	}

	rep, err := f.remapper(true).Remap(ctx)
	if err != nil {
		t.Fatalf("remap: %v (report %+v)", err, rep)
	}
	if rep.Remapped != 2 || len(rep.Unmappable) != 0 {
		t.Errorf("report = %+v, want both tracks remapped", rep)
	}
	// rating restored and readable
	if got := f.plex.ratings[newA]; got != 8 {
		t.Errorf("rating on the new key = %v, want 8", got)
	}
	// the non-mirror playlist holds the track again, under its new key
	var roadKey string
	for k, p := range f.plex.playlists {
		if p.Title == "Road Trip" {
			roadKey = k
		}
	}
	if roadKey == "" || fmt.Sprint(f.plex.playlists[roadKey].Keys) != fmt.Sprint([]string{newA}) {
		t.Errorf("Road Trip = %v, want [%s]", f.plex.playlists[roadKey], newA)
	}
	// the mirror exists again with both songs under new keys
	newB := f.plex.keyFor(f.plex.toPlex(mustPath(t, f, f.itemB)))
	if got := f.mirrorKeys(); fmt.Sprint(got) != fmt.Sprint(sortedKeys(newA, newB)) {
		t.Errorf("mirror = %v, want %v", got, sortedKeys(newA, newB))
	}
	// the database learned the new keys, and the membership points at the new playlist
	var key string
	_ = f.v.db.QueryRow(`SELECT plex_rating_key FROM mus_track_identities WHERE recording_id = ?`, f.v.item(f.itemA).RecordingID).Scan(&key)
	if key != newA {
		t.Errorf("identity key = %q, want %q", key, newA)
	}
	if n := f.v.count(`SELECT COUNT(*) FROM mus_track_memberships WHERE plex_playlist_key = ? AND is_mirror = 0`, roadKey); n != 1 {
		t.Errorf("memberships on the new Road Trip key = %d, want 1", n)
	}
	// running it again changes nothing
	rep2, err := f.remapper(true).Remap(ctx)
	if err != nil || rep2.Remapped != 0 || rep2.Unchanged != 2 {
		t.Errorf("second remap = %+v %v, want all unchanged", rep2, err)
	}
}

func mustPath(t *testing.T, f *mirrorFix, itemID int64) string {
	t.Helper()
	var p string
	if err := f.v.db.QueryRow(`SELECT path FROM mus_library_tracks WHERE id = ?`, f.v.item(itemID).TrackID).Scan(&p); err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(strings.Replace(p, f.plexRt, f.lib, 1))
}

func TestRemapUnmappableTrackGetsNoWritesAndADecision(t *testing.T) {
	f := newMirrorFix(t)
	ctx := context.Background()
	keyB := f.keyOf(f.itemB)
	road, _ := f.plex.CreatePlaylist(ctx, "Road Trip", []string{keyB})
	_ = f.plex.SetRating(ctx, keyB, 6)
	f.recordState(f.itemB, 6, road, "Road Trip")

	// The file changed after the identity was recorded, so Plex's track cannot be proven the same.
	pathB := mustPath(t, f, f.itemB)
	placeWrite(t, pathB, "REPLACED-AUDIO", TrackTags{Artist: "Band", Album: "Album", Title: "Song B", Disc: 1, Track: 2})

	f.plex.reimport("Road Trip")
	rep, err := f.remapper(false).Remap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unmappable) != 1 || rep.Remapped != 1 { // song A (no recorded extras) remaps; song B does not
		t.Fatalf("report = %+v, want one unmappable and one remapped", rep)
	}
	newB := f.plex.keyFor(f.plex.toPlex(pathB))
	if _, ok := f.plex.ratings[newB]; ok {
		t.Error("a rating was written for the unmappable track")
	}
	for _, p := range f.plex.playlists {
		if len(p.Keys) != 0 {
			t.Errorf("playlist %q was written to: %v", p.Title, p.Keys)
		}
	}
	if n := f.v.count(`SELECT COUNT(*) FROM mus_decisions WHERE item_id = ? AND kind = 'remap' AND answered_at = 0`, f.itemB); n != 1 {
		t.Errorf("remap decisions = %d, want 1", n)
	}
	var key string
	_ = f.v.db.QueryRow(`SELECT plex_rating_key FROM mus_track_identities WHERE recording_id = ?`, f.v.item(f.itemB).RecordingID).Scan(&key)
	if key != keyB {
		t.Errorf("identity key changed to %q for an unmappable track", key)
	}
}

func TestRemapPlaylistThatNoLongerExistsMeansNoWritesForThatTrack(t *testing.T) {
	f := newMirrorFix(t)
	ctx := context.Background()
	keyA := f.keyOf(f.itemA)
	road, _ := f.plex.CreatePlaylist(ctx, "Road Trip", []string{keyA})
	_ = f.plex.SetRating(ctx, keyA, 9)
	f.recordState(f.itemA, 9, road, "Road Trip")
	f.plex.reimport() // the user's playlist did not survive
	rep, err := f.remapper(false).Remap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unmappable) != 1 || !strings.Contains(rep.Unmappable[0], "no longer exists") {
		t.Errorf("report = %+v, want the missing playlist named", rep)
	}
	if len(f.plex.ratings) != 0 {
		t.Errorf("ratings written despite the unresolved playlist: %v", f.plex.ratings)
	}
}
