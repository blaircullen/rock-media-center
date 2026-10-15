package musicengine

import (
	"context"
	"strings"
	"testing"

	"github.com/tristenlammi/arrmada/internal/audiocheck"
	"github.com/tristenlammi/arrmada/internal/plex"
)

// albumFakeLibrary lets every track through, except titles named in blocked.
type albumFakeLibrary struct{ blocked map[string]string }

func (f albumFakeLibrary) TrackBlocked(_ context.Context, rec Recording) (string, error) {
	return f.blocked[rec.Title], nil
}

// Full-album placement asks the library check about every track but the single and skips
// the ones it blocks, with the reason recorded (Astra 9).
func TestAlbumSkipsTracksTheLibraryCheckBlocks(t *testing.T) {
	r, _, s := newAlbumFix(t, placeSeed{
		Title: "Song", Artist: "Band", Album: "Album", Disc: 1, Track: 1,
		Others: []placeOther{{Title: "Other One", Disc: 1, Track: 2}, {Title: "Other Two", Disc: 1, Track: 3}},
	}, nil)
	a, err := NewAlbum(AlbumConfig{Placement: r.pl, Releases: placeFakeReleases{rel: placeRelease(3)},
		Library: albumFakeLibrary{blocked: map[string]string{"Other One": "already in the library as /x.flac"}}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Place(context.Background(), s.ItemID, AlbumOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Placed) != 1 || !strings.HasSuffix(res.Placed[0], "03. Other Two.flac") {
		t.Errorf("placed = %v, want only Other Two", res.Placed)
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Reason, "already in the library") {
		t.Errorf("skipped = %+v, want Other One with the library reason", res.Skipped)
	}
}

func TestAlbumWithoutALibraryCheckPlacesNothing(t *testing.T) {
	r, _, s := newAlbumFix(t, placeSeed{
		Title: "Song", Artist: "Band", Album: "Album", Disc: 1, Track: 1,
		Others: []placeOther{{Title: "Other One", Disc: 1, Track: 2}},
	}, nil)
	a, _ := NewAlbum(AlbumConfig{Placement: r.pl, Releases: placeFakeReleases{rel: placeRelease(2)}})
	if _, err := a.Place(context.Background(), s.ItemID, AlbumOptions{}); err == nil {
		t.Fatal("placing with no library check wired must fail closed")
	}
	if got := r.libFlacs(); len(got) != 1 {
		t.Errorf("library = %v, want only the single", got)
	}
}

// The real engine check behind the album: an existing genuine copy, a lossy copy and an
// owning item each block the track; a recording nobody has does not.
func TestEngineTrackBlocked(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()
	v.mb.add(mbRec("rec-1", "Song", "Artist", "", "ISRC1"))
	recID := v.seedRecording("rec-1", "Song", "Artist", "ISRC1", "studio")
	rec := Recording{ID: recID, MBID: "rec-1", Title: "Song", Artist: "Artist", ISRC: "ISRC1", VersionHint: "studio"}

	if why, err := v.e.TrackBlocked(ctx, rec); err != nil || why != "" {
		t.Fatalf("empty library: %q %v, want free to place", why, err)
	}

	id := v.seedItem(KindRequest, StateSearching, recID)
	if why, _ := v.e.TrackBlocked(ctx, rec); !strings.Contains(why, "already owns") {
		t.Errorf("owned recording: %q", why)
	}
	if _, err := v.db.Exec(`UPDATE mus_items SET state = 'done' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}

	mp3 := "/mnt/nas/music/Artist/Song album/01. Song.mp3"
	v.plex.search["artist|song"] = append(v.plex.search["artist|song"],
		plex.Track{RatingKey: "1", Title: "Song", Artist: "Artist", Album: "Song album", Files: []string{mp3}})
	v.tags.tags[mp3] = Tags{RecordingMBID: "rec-1"}
	v.checker.results[mp3] = []audiocheck.Result{verdictResult(audiocheck.Genuine, "mp3")}
	if why, _ := v.e.TrackBlocked(ctx, rec); !strings.Contains(why, "lossy or flagged") || !strings.Contains(why, "deferred") {
		t.Errorf("lossy copy: %q, want blocked with the swap deferral", why)
	}

	flac := "/mnt/nas/music/Artist/Song album/02. Song.flac"
	v.plex.search["artist|song"] = append(v.plex.search["artist|song"],
		plex.Track{RatingKey: "2", Title: "Song", Artist: "Artist", Album: "Song album", Files: []string{flac}})
	v.tags.tags[flac] = Tags{RecordingMBID: "rec-1"}
	v.checker.results[flac] = []audiocheck.Result{genuineFLAC()}
	if why, _ := v.e.TrackBlocked(ctx, rec); !strings.Contains(why, "already in the library") {
		t.Errorf("genuine copy: %q", why)
	}
}
