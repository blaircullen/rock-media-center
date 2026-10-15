package musicengine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tristenlammi/arrmada/internal/metadata"
)

type placeFakeReleases struct {
	rel *metadata.ReleaseDetails
	err error
}

func (f placeFakeReleases) GetRelease(context.Context, string) (*metadata.ReleaseDetails, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rel, nil
}

func placeRelease(tracks int) *metadata.ReleaseDetails {
	d := &metadata.ReleaseDetails{Artist: "Band", Tracks: make([]metadata.ReleaseTrack, tracks)}
	d.MBID, d.Title, d.Status, d.Country, d.Date = "rel-mbid", "Album", "Official", "US", "2012"
	return d
}

func newAlbumFix(t *testing.T, s placeSeed, sub func(context.Context, AlbumRelease) (*AlbumRelease, error)) (*placeRig, *Album, placeSeeded) {
	t.Helper()
	r := newPlaceRig(t)
	seeded := r.seed(s)
	r.drive(seeded.ItemID, 5) // the single is placed first
	a, err := NewAlbum(AlbumConfig{Placement: r.pl, Releases: placeFakeReleases{rel: placeRelease(4)}, Substitute: sub})
	if err != nil {
		t.Fatal(err)
	}
	return r, a, seeded
}

func TestAlbumPlacesTheRestKeepsTheSingleAndDiscardsItsAlbumCopy(t *testing.T) {
	r, a, s := newAlbumFix(t, placeSeed{
		Title: "Song", Artist: "Band", Album: "Album", Disc: 1, Track: 1,
		Others: []placeOther{
			{Title: "Other One", Disc: 1, Track: 2},
			{Title: "Other Two", Disc: 1, Track: 3},
			{Title: "Fake One", Disc: 1, Track: 4, Audio: "FAKE-AUDIO"},
		},
	}, nil)
	single := filepath.Join(r.lib, "Band", "Album", "01. Song.flac")
	before, _ := placeSHA256(single)

	res, err := a.Place(context.Background(), s.ItemID, AlbumOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.NeedsConfirmation {
		t.Fatal("asked for confirmation although the release is still in staging")
	}
	wantPlaced := []string{filepath.Join(r.lib, "Band", "Album", "02. Other One.flac"), filepath.Join(r.lib, "Band", "Album", "03. Other Two.flac")}
	if fmt.Sprint(res.Placed) != fmt.Sprint(wantPlaced) {
		t.Errorf("placed = %v, want %v", res.Placed, wantPlaced)
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Reason, "FAKE") {
		t.Errorf("skipped = %+v, want the fake-shelf track with its verdict", res.Skipped)
	}
	if len(res.Discarded) != 1 || len(res.Better) != 0 {
		t.Errorf("discarded %v better %v, want the album's copy of the single discarded", res.Discarded, res.Better)
	}
	after, _ := placeSHA256(single)
	if before != after {
		t.Error("the placed single was touched")
	}
	if got := r.libFlacs(); len(got) != 3 {
		t.Errorf("library = %v, want the single plus two album tracks", got)
	}
	if left := placeTemps(t, r.lib); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
	if n := r.openRows(); n != 0 {
		t.Errorf("%d journal rows left open", n)
	}
	if res.Plan.Pinned.String() != "Band - Album (Official US 2012), 4 tracks" {
		t.Errorf("pinned release = %q", res.Plan.Pinned.String())
	}
	// Running it again places nothing new and does not duplicate.
	res2, err := a.Place(context.Background(), s.ItemID, AlbumOptions{})
	if err != nil || len(res2.Placed) != 2 || len(r.libFlacs()) != 3 {
		t.Errorf("second run: %+v %v, library %v", res2, err, r.libFlacs())
	}
}

func TestAlbumNumbersAcrossDiscsAndFilesCompilationsUnderTheCompilation(t *testing.T) {
	r, a, s := newAlbumFix(t, placeSeed{
		Title: "Opener", Artist: "Artist One", AlbumArtist: "Various Artists", Album: "Now Hits", Disc: 1, Track: 1,
		Others: []placeOther{
			{Title: "Second", Disc: 1, Track: 2, Artist: "Artist Two"},
			{Title: "Third", Disc: 1, Track: 3, Artist: "Artist Three"},
			{Title: "Disc Two Start", Disc: 2, Track: 1, Artist: "Artist Four"},
		},
	}, nil)
	res, err := a.Place(context.Background(), s.ItemID, AlbumOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The single itself went to Various Artists, so the album joins it there.
	want := filepath.Join(r.lib, "Various Artists", "Now Hits", "04. Disc Two Start.flac")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected %s: %v (placed %v, skipped %+v)", want, err, res.Placed, res.Skipped)
	}
	tags, _ := r.ed.Read(context.Background(), want)
	if tags.Disc != 2 || tags.Track != 4 {
		t.Errorf("tags = disc %d track %d, want disc 2 track 4 (continuous)", tags.Disc, tags.Track)
	}
	if _, err := os.Stat(filepath.Join(r.lib, "Artist Four")); err == nil {
		t.Error("a folder was created for a track artist instead of the compilation")
	}
}

func TestAlbumAfterStagingExpiryShowsSubstituteAndPlacesNothingUntilConfirmed(t *testing.T) {
	sub := func(_ context.Context, pinned AlbumRelease) (*AlbumRelease, error) {
		s := pinned
		s.Edition, s.TrackCount = "Official GB 2013 (deluxe)", 6
		return &s, nil
	}
	r, a, s := newAlbumFix(t, placeSeed{
		Title: "Song", Artist: "Band", Album: "Album", Disc: 1, Track: 1,
		Others: []placeOther{{Title: "Other One", Disc: 1, Track: 2}},
	}, sub)
	if err := os.RemoveAll(filepath.Dir(s.Src)); err != nil { // the 7-day staging expiry
		t.Fatal(err)
	}
	filesBefore := placeTree(t, r.lib)

	res, err := a.Place(context.Background(), s.ItemID, AlbumOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.NeedsConfirmation || !res.DecisionRaised || len(res.Placed) != 0 {
		t.Fatalf("result = %+v, want a decision and nothing placed", res)
	}
	var reason string
	var decID int64
	if err := r.v.db.QueryRow(`SELECT id, reason FROM mus_decisions WHERE item_id = ? AND kind = ? AND answered_at = 0`, s.ItemID, DecisionAlbumSubstitute).Scan(&decID, &reason); err != nil {
		t.Fatalf("no album decision: %v", err)
	}
	if !strings.Contains(reason, "Band - Album (Official GB 2013 (deluxe)), 6 tracks") {
		t.Errorf("decision reason = %q, want the substitute release spelled out", reason)
	}
	if after := placeTree(t, r.lib); fmt.Sprint(after) != fmt.Sprint(filesBefore) {
		t.Error("the library changed before Blair confirmed")
	}
	// Asking again does not pile up decisions.
	if _, err := a.Place(context.Background(), s.ItemID, AlbumOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := r.v.count(`SELECT COUNT(*) FROM mus_decisions WHERE item_id = ? AND kind = ?`, s.ItemID, DecisionAlbumSubstitute); n != 1 {
		t.Errorf("decisions = %d, want 1", n)
	}

	// Confirmed without a fetched release: refused, nothing placed.
	if _, err := a.ConfirmAlbum(context.Background(), decID, filepath.Join(r.stage, "nothing")); err == nil {
		t.Error("confirm with an empty staging dir succeeded")
	}
	// Blair confirms; the substitute release has been fetched.
	dir := filepath.Join(r.stage, "substitute")
	tags := func(title string, n int) TrackTags {
		return TrackTags{Artist: "Band", Album: "Album", Title: title, Disc: 1, Track: n, RecordingMBID: "mbid-" + title}
	}
	placeWrite(t, filepath.Join(dir, "01.flac"), "AUDIO-Song", tags("Song", 1))
	placeWrite(t, filepath.Join(dir, "02.flac"), "AUDIO-Other One", tags("Other One", 2))
	res, err = a.ConfirmAlbum(context.Background(), decID, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Placed) != 1 || len(res.Discarded) != 1 {
		t.Errorf("result = %+v, want one track placed and the single's copy discarded", res)
	}
	if n := r.v.count(`SELECT COUNT(*) FROM mus_decisions WHERE id = ? AND answered_at > 0`, decID); n != 1 {
		t.Error("decision was not closed after placement")
	}
}

func TestAlbumKeepsSingleAndReportsAStrictlyBetterAlbumCopy(t *testing.T) {
	r, a, s := newAlbumFix(t, placeSeed{Title: "Song", Artist: "Band", Album: "Album", Disc: 1, Track: 1}, nil)
	if err := os.RemoveAll(filepath.Dir(s.Src)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(r.stage, "hires")
	placeWrite(t, filepath.Join(dir, "01.flac"), "HIRES-AUDIO-Song", TrackTags{Artist: "Band", Album: "Album", Title: "Song", Disc: 1, Track: 1, RecordingMBID: "mbid-Song"})
	res, err := a.Place(context.Background(), s.ItemID, AlbumOptions{Confirmed: true, StagingDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Better) != 1 || len(res.Discarded) != 0 || len(res.Placed) != 0 {
		t.Errorf("result = %+v, want the better copy reported, not placed", res)
	}
	if got := r.libFlacs(); len(got) != 1 {
		t.Errorf("library = %v, want only the original single", got)
	}
}

func TestAlbumNeedsAPlacedSingleAndARelease(t *testing.T) {
	r := newPlaceRig(t)
	a, _ := NewAlbum(AlbumConfig{Placement: r.pl, Releases: placeFakeReleases{rel: placeRelease(1)}})
	s := r.seed(placeSeed{Title: "Song", Artist: "Band", Album: "Album", Disc: 1, Track: 1})
	if _, err := a.Plan(context.Background(), s.ItemID); err == nil || !strings.Contains(err.Error(), "placed first") {
		t.Errorf("plan on an unplaced item: %v", err)
	}
	r.drive(s.ItemID, 5)
	none, _ := NewAlbum(AlbumConfig{Placement: r.pl})
	if _, err := none.Plan(context.Background(), s.ItemID); !errors.Is(err, ErrAlbumUnavailable) {
		t.Errorf("plan with no release lookup: %v, want ErrAlbumUnavailable", err)
	}
}
