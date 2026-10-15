package musicengine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tristenlammi/arrmada/internal/metadata"
	"github.com/tristenlammi/arrmada/internal/spotify"
)

func (v *env) recordingMBID(id int64) string {
	v.t.Helper()
	if id == 0 {
		return ""
	}
	var mbid string
	if err := v.db.QueryRow(`SELECT mbid FROM mus_recordings WHERE id = ?`, id).Scan(&mbid); err != nil {
		v.t.Fatal(err)
	}
	return mbid
}

func TestISRCResolvingToStudioAndAcousticPinsTheStudioRecording(t *testing.T) {
	v := newEnv(t) // AE1
	studio := mbRec("rec-studio", "Hang With Me", "Robyn", "", "SEXXX1000001")
	studio.Releases = []metadata.ReleaseRef{{MBID: "rel-pt2", Title: "Body Talk Pt. 2", Status: "Official", Date: "2010-09-06", ReleaseGroupMBID: "rg-pt2"}}
	acoustic := mbRec("rec-acoustic", "Hang With Me", "Robyn", "acoustic version", "SEXXX1000001")
	acoustic.Releases = []metadata.ReleaseRef{{MBID: "rel-pt1", Title: "Body Talk Pt. 1", Status: "Official", Date: "2010-06-14", ReleaseGroupMBID: "rg-pt1"}}
	v.mb.add(acoustic)
	v.mb.add(studio)
	v.spotify.set(entry("sp1", "Hang With Me", "Robyn", "SEXXX1000001"))
	v.sync()

	v.tick(2)

	it := v.itemFor("sp1")
	if it.State != StateLibraryCheck {
		t.Fatalf("state = %s (%s), want library_check", it.State, it.LastReason)
	}
	if got := v.recordingMBID(it.RecordingID); got != "rec-studio" {
		t.Fatalf("pinned %q, want the studio recording", got)
	}
	var rel, hint string
	if err := v.db.QueryRow(`SELECT release_mbid, version_hint FROM mus_recordings WHERE id = ?`, it.RecordingID).Scan(&rel, &hint); err != nil {
		t.Fatal(err)
	}
	if rel != "rel-pt2" || hint != "studio" {
		t.Fatalf("release %q hint %q, want the Pt. 2 release and studio", rel, hint)
	}
	if v.count(`SELECT COUNT(*) FROM mus_playlist_entries WHERE recording_id = ?`, it.RecordingID) != 1 {
		t.Fatal("the entry should carry the recording id")
	}
	for _, c := range v.mb.classes {
		if c != metadata.MBIntake {
			t.Fatalf("MusicBrainz class %v, want intake priority (R32)", c)
		}
	}
}

func TestAcousticSpotifyTitlePinsTheAcousticRecording(t *testing.T) {
	v := newEnv(t)
	v.mb.add(mbRec("rec-studio", "Hang With Me", "Robyn", "", "I1"))
	v.mb.add(mbRec("rec-acoustic", "Hang With Me", "Robyn", "acoustic version", "I1"))
	v.spotify.set(entry("sp1", "Hang With Me - Acoustic", "Robyn", "I1"))
	v.sync()
	v.tick(2)
	if got := v.recordingMBID(v.itemFor("sp1").RecordingID); got != "rec-acoustic" {
		t.Fatalf("pinned %q, want acoustic", got)
	}
}

// A relinked entry with exactly one fitting recording is pinned like any other (Blair,
// 2026-10-15).
func TestRelinkedEntryWithOneMatchIsPinned(t *testing.T) {
	v := newEnv(t)
	v.mb.add(mbRec("rec-1", "Song", "Artist", "", "I1"))
	e := entry("sp1", "Song", "Artist", "I1")
	e.LinkedFromID = "spOriginal"
	v.spotify.set(e)
	v.sync()
	v.tick(2)
	if it := v.itemFor("sp1"); it.State != StateLibraryCheck || v.recordingMBID(it.RecordingID) != "rec-1" {
		t.Fatalf("state %s reason %q, want pinned to rec-1", it.State, it.LastReason)
	}
}

func TestAmbiguousRelinkedEntryGoesToBlairOnceEvenIfRunTwice(t *testing.T) {
	v := newEnv(t)
	v.mb.add(mbRec("rec-1", "Song", "Artist", "", "I1"))
	v.mb.add(mbRec("rec-2", "Song", "Artist", "", "I1"))
	e := entry("sp1", "Song", "Artist", "I1")
	e.LinkedFromID = "spOriginal"
	v.spotify.set(e)
	v.sync()
	v.tick(2)

	it := v.itemFor("sp1")
	if it.State != StateWaitingBlair || !strings.Contains(it.LastReason, "swapped this track") {
		t.Fatalf("state %s reason %q, want waiting_blair with a relinked reason", it.State, it.LastReason)
	}
	if it.DeadlineAt != 0 {
		t.Fatal("waiting_blair must suspend the deadline")
	}
	if v.openDecisions(it.ID) != 1 {
		t.Fatalf("open decisions = %d, want 1", v.openDecisions(it.ID))
	}
	var kind, opts string
	if err := v.db.QueryRow(`SELECT kind, options FROM mus_decisions WHERE item_id = ?`, it.ID).Scan(&kind, &opts); err != nil {
		t.Fatal(err)
	}
	if kind != "ambiguous" || !strings.Contains(opts, "use:rec-1") || !strings.Contains(opts, "drop") {
		t.Fatalf("decision %s %s", kind, opts)
	}

	// Forced re-run of the handler (a crash replay): still one open decision.
	if _, err := v.db.Exec(`UPDATE mus_items SET state = 'pinning' WHERE id = ?`, it.ID); err != nil {
		t.Fatal(err)
	}
	v.tick(1)
	if v.openDecisions(it.ID) != 1 || v.count(`SELECT COUNT(*) FROM mus_decisions`) != 1 {
		t.Fatal("raising twice must leave one decision")
	}
	if v.item(it.ID).State != StateWaitingBlair {
		t.Fatal("replayed pin should land in waiting_blair again")
	}
}

func TestLocalFileGoesToBlair(t *testing.T) {
	v := newEnv(t)
	v.spotify.set(spotify.Entry{Type: "track", IsLocal: true, Name: "Demo", Artists: []string{"Me"}, DurationMs: 100000})
	v.sync()
	v.tick(2)
	var id int64
	_ = v.db.QueryRow(`SELECT item_id FROM mus_playlist_entries`).Scan(&id)
	it := v.item(id)
	if it.State != StateWaitingBlair || !strings.Contains(it.LastReason, "local") {
		t.Fatalf("state %s reason %q", it.State, it.LastReason)
	}
}

// Astra #12: an identified track Spotify marks unavailable is not pinned and searched, even
// when MusicBrainz has exactly one match; it goes to Blair with the real reason.
func TestUnavailableTrackGoesToBlair(t *testing.T) {
	v := newEnv(t)
	v.mb.search = []metadata.Recording{mbRec("rec-1", "Song", "Artist", "", "")}
	e := entry("sp1", "Song", "Artist", "")
	e.Unavailable = true
	v.spotify.set(e)
	v.sync()
	v.tick(2)
	if it := v.itemFor("sp1"); it.State != StateWaitingBlair || !strings.Contains(it.LastReason, "unavailable") {
		t.Fatalf("state %s reason %q", it.State, it.LastReason)
	}
}

func TestNoISRCWithOneClearSearchMatchPins(t *testing.T) {
	v := newEnv(t)
	v.mb.search = []metadata.Recording{mbRec("rec-1", "Song", "Artist", "", "")}
	v.spotify.set(entry("sp1", "Song", "Artist", ""))
	v.sync()
	v.tick(2)
	if it := v.itemFor("sp1"); it.State != StateLibraryCheck || v.recordingMBID(it.RecordingID) != "rec-1" {
		t.Fatalf("state %s reason %q", it.State, it.LastReason)
	}
}

func TestNoISRCAndAmbiguousSearchGoesToBlair(t *testing.T) {
	v := newEnv(t)
	a := mbRec("rec-a", "Song", "Artist", "", "")
	b := mbRec("rec-b", "Song", "Artist", "", "")
	v.mb.search = []metadata.Recording{a, b}
	v.spotify.set(entry("sp1", "Song", "Artist", ""))
	v.sync()
	v.tick(2)
	it := v.itemFor("sp1")
	if it.State != StateWaitingBlair || !strings.Contains(it.LastReason, "match equally") {
		t.Fatalf("state %s reason %q", it.State, it.LastReason)
	}
	if v.openDecisions(it.ID) != 1 {
		t.Fatal("expected one decision")
	}
}

func TestSearchResultOfADifferentSongIsNotPinned(t *testing.T) {
	v := newEnv(t)
	v.mb.search = []metadata.Recording{mbRec("rec-x", "Other Song", "Artist", "", "")}
	v.spotify.set(entry("sp1", "Song", "Artist", ""))
	v.sync()
	v.tick(2)
	if it := v.itemFor("sp1"); it.State != StateWaitingBlair {
		t.Fatalf("state %s, want waiting_blair", it.State)
	}
}

func TestUnknownISRCFallsBackToSearch(t *testing.T) {
	v := newEnv(t)
	v.mb.search = []metadata.Recording{mbRec("rec-1", "Song", "Artist", "", "")}
	v.spotify.set(entry("sp1", "Song", "Artist", "UNKNOWNISRC"))
	v.sync()
	v.tick(2)
	if it := v.itemFor("sp1"); it.State != StateLibraryCheck {
		t.Fatalf("state %s (%s)", it.State, it.LastReason)
	}
}

func TestExplicitSpotifyEntryNeverPinsToACleanRecording(t *testing.T) {
	v := newEnv(t)
	v.mb.add(mbRec("rec-clean", "Song", "Artist", "clean", "I1"))
	e := entry("sp1", "Song", "Artist", "I1")
	e.Explicit = true
	v.spotify.set(e)
	v.sync()
	v.tick(2)
	it := v.itemFor("sp1")
	if it.State != StateWaitingBlair || !strings.Contains(it.LastReason, "explicit") {
		t.Fatalf("state %s reason %q", it.State, it.LastReason)
	}
}

func TestPinMergesIntoActiveSweepItemAndTakesIntakePriority(t *testing.T) {
	v := newEnv(t)
	v.mb.add(mbRec("rec-1", "Song", "Artist", "", "I1"))
	recID := v.seedRecording("rec-1", "Song", "Artist", "I1", "studio")
	sweep := v.seedItem(KindSweep, StateSearching, recID)

	v.spotify.set(entry("sp1", "Song", "Artist", "I1"))
	v.sync()
	req := v.itemFor("sp1")
	v.tick(2)

	if v.count(`SELECT COUNT(*) FROM mus_items WHERE recording_id = ? AND state NOT IN ('done','cancelled','dropped')`, recID) != 1 {
		t.Fatal("more than one active item holds the recording")
	}
	merged := v.item(req.ID)
	if merged.State != StateCancelled || !strings.Contains(merged.LastReason, "Merged into item") {
		t.Fatalf("request item = %s %q", merged.State, merged.LastReason)
	}
	survivor := v.item(sweep)
	if survivor.Priority != PriorityIntake {
		t.Fatalf("survivor priority = %d, want intake %d (R32)", survivor.Priority, PriorityIntake)
	}
	var pointsAt int64
	if err := v.db.QueryRow(`SELECT item_id FROM mus_playlist_entries WHERE spotify_track_id = 'sp1'`).Scan(&pointsAt); err != nil {
		t.Fatal(err)
	}
	if pointsAt != sweep {
		t.Fatalf("entry points at item %d, want the survivor %d (KTD2)", pointsAt, sweep)
	}
}

func TestTwoRequestsForOneRecordingMergeIntoOne(t *testing.T) {
	v := newEnv(t)
	v.mb.add(mbRec("rec-1", "Song", "Artist", "", "I1"))
	v.spotify.set(entry("spA", "Song", "Artist", "I1"), entry("spB", "Song", "Artist", "I1"))
	v.sync()
	v.tick(2)
	a, b := v.itemFor("spA"), v.itemFor("spB")
	if a.ID != b.ID {
		t.Fatalf("entries point at items %d and %d, want one shared item", a.ID, b.ID)
	}
	if a.State != StateLibraryCheck {
		t.Fatalf("surviving item = %s", a.State)
	}
	if v.count(`SELECT COUNT(*) FROM mus_items WHERE state = 'cancelled'`) != 1 {
		t.Fatal("expected exactly one merged-away item")
	}
}

func TestPinToRecordingAppliesBlairsChoice(t *testing.T) {
	v := newEnv(t)
	v.mb.add(mbRec("rec-a", "Song", "Artist", "", "I1"))
	b := mbRec("rec-b", "Song", "Artist", "", "I1")
	v.mb.add(b)
	v.spotify.set(entry("sp1", "Song", "Artist", "I1"))
	v.sync()
	v.tick(2) // two equal recordings: waiting_blair
	it := v.itemFor("sp1")
	if it.State != StateWaitingBlair {
		t.Fatalf("setup: state %s", it.State)
	}
	if err := v.e.PinToRecording(context.Background(), it.ID, "rec-b"); err != nil {
		t.Fatal(err)
	}
	it = v.item(it.ID)
	if it.State != StateLibraryCheck || v.recordingMBID(it.RecordingID) != "rec-b" {
		t.Fatalf("state %s recording %q", it.State, v.recordingMBID(it.RecordingID))
	}
	if err := v.e.PinToRecording(context.Background(), it.ID, "rec-b"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("pinning a pinned item err = %v, want ErrInvalidTransition", err)
	}
}

func TestVersionAndNameHelpers(t *testing.T) {
	if got := versionHints("Song (Live at Wembley) - Radio Edit"); !sameSet(got, []string{"edit", "live"}) {
		t.Errorf("hints = %v", got)
	}
	if got := versionHints("Remixed"); !sameSet(got, []string{"remix"}) {
		t.Errorf("hints = %v", got)
	}
	if normName("Song (Remastered 2011)") != normName("Song - 2011 Remaster") {
		t.Error("remaster suffixes should normalise away")
	}
	if firstArtist("A feat. B") != "A" || firstArtist("Solo") != "Solo" {
		t.Error("firstArtist")
	}
}

// A song whose Spotify album names no release still lands on the artist's own album, not an
// earlier compilation it also appears on.
func TestPickReleasePrefersTheArtistsOwnAlbumOverACompilation(t *testing.T) {
	c := metadata.Recording{Releases: []metadata.ReleaseRef{
		{MBID: "now79", ReleaseGroupMBID: "g-now", Title: "NOW 79", Status: "Official", Date: "2021-07-01",
			PrimaryType: "Album", SecondaryTypes: []string{"Compilation"}},
		{MBID: "single", ReleaseGroupMBID: "g-single", Title: "Leave Before You Love Me", Status: "Official", Date: "2021-05-21", PrimaryType: "Single"},
		{MBID: "album", ReleaseGroupMBID: "g-album", Title: "Shockwave", Status: "Official", Date: "2021-06-11", PrimaryType: "Album"},
	}}
	if rel, grp := pickRelease(c, "Some Playlist Album"); rel != "album" || grp != "g-album" {
		t.Fatalf("picked %s/%s, want the artist's album", rel, grp)
	}
	// The Spotify album title still wins when it names one.
	if rel, _ := pickRelease(c, "Leave Before You Love Me"); rel != "single" {
		t.Fatalf("picked %s, want the release Spotify names", rel)
	}
	// Only a compilation: it is used rather than nothing.
	only := metadata.Recording{Releases: c.Releases[:1]}
	if rel, _ := pickRelease(only, ""); rel != "now79" {
		t.Fatalf("picked %s, want the only release", rel)
	}
}
