package musicengine

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/tristenlammi/arrmada/internal/plex"
)

func ptrack(key, title string, rating float64, plays int) plex.Track {
	return plex.Track{RatingKey: key, Title: title, Artist: "Artist", Album: "Album",
		UserRating: rating, ViewCount: plays, Files: []string{"/mnt/nas/music/Artist/Album/" + title + ".flac"}}
}

func TestCatalogueRefreshesTracksRatingsPlaysAndPlaylistCounts(t *testing.T) {
	v := newEnv(t)
	v.plex.tracks = []plex.Track{ptrack("1", "One", 10, 5), ptrack("2", "Two", 0, 0), ptrack("3", "Three", 2, 1)}
	v.plex.playlists = []plex.Playlist{
		{RatingKey: "p1", Title: "Road"},
		{RatingKey: "p2", Title: "Gym"},
		{RatingKey: "smart", Title: "All Music", Smart: true},
	}
	v.plex.items["p1"] = []plex.PlaylistItem{{RatingKey: "1"}, {RatingKey: "2"}, {RatingKey: "1"}} // 1 twice in one list
	v.plex.items["p2"] = []plex.PlaylistItem{{RatingKey: "1"}}
	v.plex.items["smart"] = []plex.PlaylistItem{{RatingKey: "1"}, {RatingKey: "2"}, {RatingKey: "3"}}

	rep, err := v.e.RefreshCatalogue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tracks != 3 || rep.Written != 3 || rep.Inserted != 3 || rep.Playlists != 2 {
		t.Fatalf("report %+v", rep)
	}
	type row struct {
		rating   *float64
		plays, n int
	}
	get := func(key string) row {
		var r row
		if err := v.db.QueryRow(`SELECT user_rating, play_count, playlist_count FROM mus_library_tracks WHERE plex_rating_key = ?`, key).
			Scan(&r.rating, &r.plays, &r.n); err != nil {
			t.Fatal(err)
		}
		return r
	}
	one, two, three := get("1"), get("2"), get("3")
	if one.n != 2 || one.plays != 5 || one.rating == nil || *one.rating != 10 {
		t.Fatalf("track 1 = %+v, want 2 non-smart playlists (smart excluded), 5 plays, rating 10", one)
	}
	if two.n != 1 || two.rating != nil {
		t.Fatalf("track 2 = %+v, want 1 playlist and NULL (unrated)", two)
	}
	if three.n != 0 || three.rating == nil || *three.rating != 2 {
		t.Fatalf("track 3 = %+v, want 0 playlists (only in a smart one) and rating 2", three)
	}
}

func TestCatalogueSecondPassIsIdempotentAndKeepsChecks(t *testing.T) {
	v := newEnv(t)
	v.plex.tracks = []plex.Track{ptrack("1", "One", 0, 0)}
	if _, err := v.e.RefreshCatalogue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := v.db.Exec(`UPDATE mus_library_tracks SET verdict = 'GENUINE', verdict_at = 5, sha256 = 'abc' WHERE plex_rating_key = '1'`); err != nil {
		t.Fatal(err)
	}
	v.plex.tracks[0].ViewCount = 9

	rep, err := v.e.RefreshCatalogue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Inserted != 0 || v.count(`SELECT COUNT(*) FROM mus_library_tracks`) != 1 {
		t.Fatalf("report %+v, rows %d", rep, v.count(`SELECT COUNT(*) FROM mus_library_tracks`))
	}
	var verdict string
	var plays int
	_ = v.db.QueryRow(`SELECT verdict, play_count FROM mus_library_tracks`).Scan(&verdict, &plays)
	if verdict != "GENUINE" || plays != 9 {
		t.Fatalf("verdict %q plays %d: a refresh keeps checks and updates plays", verdict, plays)
	}
}

func TestCatalogueClearsStaleCheckWhenThePathChanges(t *testing.T) {
	v := newEnv(t)
	v.plex.tracks = []plex.Track{ptrack("1", "One", 0, 0)}
	_, _ = v.e.RefreshCatalogue(context.Background())
	_, _ = v.db.Exec(`UPDATE mus_library_tracks SET verdict = 'GENUINE', verdict_at = 5, sha256 = 'abc'`)
	v.plex.tracks[0].Files = []string{"/mnt/nas/music/Artist/Album/One.mp3"}
	if _, err := v.e.RefreshCatalogue(context.Background()); err != nil {
		t.Fatal(err)
	}
	var verdict, sha, path string
	_ = v.db.QueryRow(`SELECT verdict, sha256, path FROM mus_library_tracks`).Scan(&verdict, &sha, &path)
	if verdict != "" || sha != "" || path != "/mnt/nas/music/Artist/Album/One.mp3" {
		t.Fatalf("verdict %q sha %q path %q: a replaced file's old verdict must not survive", verdict, sha, path)
	}
}

func TestCatalogueIsResumableAfterAStoppedPass(t *testing.T) {
	v := newEnv(t)
	for i := 0; i < 5; i++ {
		v.plex.tracks = append(v.plex.tracks, ptrack(fmt.Sprint(i), fmt.Sprintf("T%d", i), 0, 0))
	}
	v.e.catalogueBatch = 2
	ctx, cancel := context.WithCancel(context.Background())
	v.e.afterCatalogueBatch = func(done int) {
		if done >= 2 {
			cancel() // stop the pass after the first committed batch
		}
	}
	rep, err := v.e.RefreshCatalogue(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the stop to surface", err)
	}
	if rep.Written != 2 || v.count(`SELECT COUNT(*) FROM mus_library_tracks`) != 2 {
		t.Fatalf("stopped pass: report %+v rows %d, want the finished batch kept", rep, v.count(`SELECT COUNT(*) FROM mus_library_tracks`))
	}

	v.e.afterCatalogueBatch = nil
	rep, err = v.e.RefreshCatalogue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Written != 5 || rep.Inserted != 3 || v.count(`SELECT COUNT(*) FROM mus_library_tracks`) != 5 {
		t.Fatalf("resumed pass: report %+v rows %d", rep, v.count(`SELECT COUNT(*) FROM mus_library_tracks`))
	}
}

func TestCataloguePlaylistReadFailureWritesNothing(t *testing.T) {
	v := newEnv(t)
	v.plex.tracks = []plex.Track{ptrack("1", "One", 0, 0)}
	v.plex.playlists = []plex.Playlist{{RatingKey: "p1", Title: "Road"}}
	v.plex.playlistErr = errors.New("plex 500")
	if _, err := v.e.RefreshCatalogue(context.Background()); err == nil {
		t.Fatal("a playlist read failure must fail the pass")
	}
	if v.count(`SELECT COUNT(*) FROM mus_library_tracks`) != 0 {
		t.Fatal("tracks were written with possibly wrong playlist counts")
	}
}

func TestCatalogueKeepsTracksPlexNoLongerLists(t *testing.T) {
	v := newEnv(t)
	v.plex.tracks = []plex.Track{ptrack("1", "One", 0, 0), ptrack("2", "Two", 0, 0)}
	_, _ = v.e.RefreshCatalogue(context.Background())
	v.clock.Advance(86400 * 1e9)
	v.plex.tracks = v.plex.tracks[:1]
	_, _ = v.e.RefreshCatalogue(context.Background())
	if v.count(`SELECT COUNT(*) FROM mus_library_tracks`) != 2 {
		t.Fatal("a track missing from Plex was deleted; nothing may be")
	}
	if v.count(`SELECT COUNT(*) FROM mus_library_tracks WHERE plex_rating_key = '2' AND last_seen_at < (SELECT last_seen_at FROM mus_library_tracks WHERE plex_rating_key = '1')`) != 1 {
		t.Fatal("the missing track's last_seen_at should fall behind")
	}
}

func TestCatalogueSkipsTracksWithoutAFile(t *testing.T) {
	v := newEnv(t)
	v.plex.tracks = []plex.Track{{RatingKey: "9", Title: "Ghost"}, ptrack("1", "One", 0, 0)}
	rep, err := v.e.RefreshCatalogue(context.Background())
	if err != nil || rep.Skipped != 1 || rep.Written != 1 {
		t.Fatalf("report %+v err %v", rep, err)
	}
}

func TestCatalogueNeedsASection(t *testing.T) {
	v := newEnv(t)
	v.settings.PlexSection = ""
	if _, err := v.e.RefreshCatalogue(context.Background()); err == nil {
		t.Fatal("expected an error when no section is configured")
	}
}
