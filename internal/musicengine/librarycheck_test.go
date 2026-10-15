package musicengine

import (
	"strings"
	"testing"
	"time"

	"github.com/tristenlammi/arrmada/internal/audiocheck"
	"github.com/tristenlammi/arrmada/internal/plex"
)

const songPath = "/mnt/nas/music/Artist/Song album/01. Song.flac"

// libItem seeds a pinned recording ("rec-1", Artist - Song, studio) and an item waiting in
// library_check for it.
func (v *env) libItem() (Item, int64) {
	v.t.Helper()
	v.mb.add(mbRec("rec-1", "Song", "Artist", "", "ISRC1"))
	recID := v.seedRecording("rec-1", "Song", "Artist", "ISRC1", "studio")
	id := v.seedItem(KindRequest, StateLibraryCheck, recID)
	return v.item(id), recID
}

func (v *env) plexHas(path, ratingKey string) {
	v.plex.search["artist|song"] = append(v.plex.search["artist|song"],
		plex.Track{RatingKey: ratingKey, Title: "Song", Artist: "Artist", Album: "Song album", Files: []string{path}})
}

func TestGenuineLibraryCopyEndsTheItemWithoutSearching(t *testing.T) {
	v := newEnv(t) // AE2
	it, recID := v.libItem()
	v.plexHas(songPath, "100")
	v.tags.tags[songPath] = Tags{RecordingMBID: "rec-1"}
	v.checker.results[songPath] = []audiocheck.Result{genuineFLAC()}

	v.tick(3)

	got := v.item(it.ID)
	if got.State != StateDone || !strings.Contains(got.LastReason, songPath) {
		t.Fatalf("state %s reason %q, want done citing the library file", got.State, got.LastReason)
	}
	if got.TrackID == 0 {
		t.Fatal("the item must note the library track for the mirror")
	}
	var verdict string
	var rec int64
	if err := v.db.QueryRow(`SELECT verdict, recording_id FROM mus_library_tracks WHERE id = ?`, got.TrackID).Scan(&verdict, &rec); err != nil {
		t.Fatal(err)
	}
	if verdict != "GENUINE" || rec != recID {
		t.Fatalf("library row verdict %q recording %d", verdict, rec)
	}
	if len(v.acquirer.calls) != 0 {
		t.Fatalf("acquirer ran %d times, want no download or search (R7)", len(v.acquirer.calls))
	}
}

func TestLiveLibraryCopyDoesNotMatchAStudioRequest(t *testing.T) {
	v := newEnv(t) // AE4
	it, _ := v.libItem()
	live := mbRec("rec-live", "Song", "Artist", "live at Red Rocks", "ISRCLIVE")
	live.LengthMs = 305000
	v.mb.add(live)
	livePath := "/mnt/nas/music/Artist/Live/01. Song.flac"
	v.plexHas(livePath, "200")
	v.tags.tags[livePath] = Tags{RecordingMBID: "rec-live"}
	v.checker.results[livePath] = []audiocheck.Result{genuineFLAC()}

	v.tick(1)

	got := v.item(it.ID)
	if got.State != StateSearching || got.Kind != KindRequest || got.TrackID != 0 {
		t.Fatalf("item = %s kind %s track %d, want a plain searching request", got.State, got.Kind, got.TrackID)
	}
	if v.checker.calls[livePath] != 0 {
		t.Fatal("the live file must not even be audio-checked as a match")
	}
}

func TestDifferentRecordingIdOfTheSameSongStillMatchesByISRC(t *testing.T) {
	v := newEnv(t)
	it, _ := v.libItem()
	dup := mbRec("rec-dup", "Song", "Artist", "", "ISRC1")
	v.mb.add(dup)
	v.plexHas(songPath, "100")
	v.tags.tags[songPath] = Tags{RecordingMBID: "rec-dup"}
	v.checker.results[songPath] = []audiocheck.Result{genuineFLAC()}
	v.tick(1)
	if got := v.item(it.ID).State; got != StateDone {
		t.Fatalf("state %s, want done: rec-dup is the same recording by ISRC", got)
	}
}

func TestSuspectLibraryCopyTurnsTheRequestIntoAnUpgrade(t *testing.T) {
	v := newEnv(t) // AE3
	it, _ := v.libItem()
	v.plexHas(songPath, "100")
	v.tags.tags[songPath] = Tags{ISRC: "ISRC1"}
	v.checker.results[songPath] = []audiocheck.Result{verdictResult(audiocheck.Suspect, "flac")}

	v.tick(1)

	got := v.item(it.ID)
	if got.State != StateSearching || got.Kind != KindUpgrade || got.TrackID == 0 {
		t.Fatalf("item = %s kind %s track %d, want searching / upgrade / tied to the track", got.State, got.Kind, got.TrackID)
	}
	if !strings.Contains(got.LastReason, "SUSPECT") {
		t.Fatalf("reason %q", got.LastReason)
	}
	var verdict string
	_ = v.db.QueryRow(`SELECT verdict FROM mus_library_tracks WHERE id = ?`, got.TrackID).Scan(&verdict)
	if verdict != "SUSPECT" {
		t.Fatalf("library verdict %q", verdict)
	}
}

func TestGenuineButLossyLibraryCopyIsAnUpgrade(t *testing.T) {
	v := newEnv(t)
	it, _ := v.libItem()
	mp3 := "/mnt/nas/music/Artist/Song album/01. Song.mp3"
	v.plexHas(mp3, "100")
	v.tags.tags[mp3] = Tags{ISRC: "ISRC1"}
	v.checker.results[mp3] = []audiocheck.Result{verdictResult(audiocheck.Genuine, "mp3")}
	v.tick(1)
	got := v.item(it.ID)
	if got.State != StateSearching || got.Kind != KindUpgrade || !strings.Contains(got.LastReason, "lossy") {
		t.Fatalf("item = %s kind %s reason %q", got.State, got.Kind, got.LastReason)
	}
}

func TestBlindLibraryReadStaysInLibraryCheckAndRetries(t *testing.T) {
	v := newEnv(t) // AE6 (the engine side: a hiccup is retried, never an upgrade)
	it, _ := v.libItem()
	v.plexHas(songPath, "100")
	v.tags.tags[songPath] = Tags{RecordingMBID: "rec-1"}
	blind := audiocheck.Result{Verdict: audiocheck.Blind, Reason: "i/o error reading the share"}
	v.checker.results[songPath] = []audiocheck.Result{blind, genuineFLAC()}

	v.tick(1)
	got := v.item(it.ID)
	if got.State != StateLibraryCheck || got.Attempts != 1 || got.NextAttemptAt <= v.clock.Now().Unix() {
		t.Fatalf("after a blind read: %+v", got)
	}
	if !strings.Contains(got.LastReason, "retrying") {
		t.Fatalf("reason %q", got.LastReason)
	}
	if len(v.acquirer.calls) != 0 {
		t.Fatal("a blind read must never start a search or upgrade")
	}

	v.tick(1) // not due yet
	if v.checker.calls[songPath] != 1 {
		t.Fatalf("checker called %d times before the retry time", v.checker.calls[songPath])
	}

	v.clock.Advance(10 * time.Minute)
	v.tick(1)
	if got := v.item(it.ID).State; got != StateDone {
		t.Fatalf("after the retry state = %s, want done", got)
	}
}

func TestLibraryFileThatStaysBlindGoesToBlairAsARecoveryDecision(t *testing.T) {
	v := newEnv(t)
	it, _ := v.libItem()
	v.plexHas(songPath, "100")
	v.tags.tags[songPath] = Tags{RecordingMBID: "rec-1"}
	v.checker.results[songPath] = []audiocheck.Result{{Verdict: audiocheck.Blind, Reason: "timeout"}}

	for i := 0; i < maxBlindAttempts+1; i++ {
		v.tick(1)
		v.clock.Advance(7 * time.Hour)
	}
	got := v.item(it.ID)
	if got.State != StateWaitingBlair {
		t.Fatalf("state = %s, want waiting_blair after %d blind reads (R14)", got.State, maxBlindAttempts)
	}
	var kind string
	if err := v.db.QueryRow(`SELECT kind FROM mus_decisions WHERE item_id = ?`, it.ID).Scan(&kind); err != nil || kind != "blind_library" {
		t.Fatalf("decision kind %q err %v, want blind_library", kind, err)
	}
	if len(v.acquirer.calls) != 0 {
		t.Fatal("unreadable audio is a recovery decision, never an upgrade")
	}
}

func TestNothingInPlexGoesToSearching(t *testing.T) {
	v := newEnv(t)
	it, _ := v.libItem()
	v.tick(1)
	got := v.item(it.ID)
	if got.State != StateSearching || !strings.Contains(got.LastReason, "not in the library") {
		t.Fatalf("state %s reason %q", got.State, got.LastReason)
	}
}

func TestUnreadableTagsRetryInsteadOfDeclaringTheSongMissing(t *testing.T) {
	v := newEnv(t)
	it, _ := v.libItem()
	v.plexHas(songPath, "100") // no tags registered: ReadTags fails
	v.tick(1)
	got := v.item(it.ID)
	if got.State != StateLibraryCheck || got.Attempts != 1 || !strings.Contains(got.LastReason, "unreadable") {
		t.Fatalf("state %s attempts %d reason %q, want a retry (a duplicate download would follow otherwise)", got.State, got.Attempts, got.LastReason)
	}
}

func TestPlexSearchErrorKeepsTheItemAndBacksOff(t *testing.T) {
	v := newEnv(t)
	it, _ := v.libItem()
	v.plex.searchErr = errPlexDown
	v.tick(1)
	got := v.item(it.ID)
	if got.State != StateLibraryCheck || got.Attempts != 1 || !strings.Contains(got.LastReason, "plex down") {
		t.Fatalf("state %s attempts %d reason %q", got.State, got.Attempts, got.LastReason)
	}
}

type constErr string

func (e constErr) Error() string { return string(e) }

const errPlexDown = constErr("plex down: connection refused")

func TestTaglessFileMatchesByAlbumAndDuration(t *testing.T) {
	v := newEnv(t)
	it, _ := v.libItem()
	v.plexHas(songPath, "100")
	v.tags.tags[songPath] = Tags{Title: "Song", Artist: "Artist", Album: "Song album", DurationSec: 200.4}
	v.checker.results[songPath] = []audiocheck.Result{genuineFLAC()}
	v.tick(1)
	if got := v.item(it.ID).State; got != StateDone {
		t.Fatalf("state %s (%s)", got, v.item(it.ID).LastReason)
	}
}

func TestTaglessLiveAlbumTrackIsNotAMatch(t *testing.T) {
	v := newEnv(t)
	it, _ := v.libItem()
	v.plexHas(songPath, "100")
	v.tags.tags[songPath] = Tags{Title: "Song (Live)", Artist: "Artist", Album: "Live at Red Rocks", DurationSec: 305}
	v.checker.results[songPath] = []audiocheck.Result{genuineFLAC()}
	v.tick(1)
	if got := v.item(it.ID).State; got != StateSearching {
		t.Fatalf("state %s, want searching", got)
	}
}

func TestTaglessFileWithAnUnknownAlbumGoesToBlair(t *testing.T) {
	v := newEnv(t)
	it, _ := v.libItem()
	v.plexHas(songPath, "100")
	v.tags.tags[songPath] = Tags{Title: "Song", Artist: "Artist", Album: "Some Compilation", DurationSec: 200}
	v.tick(1)
	got := v.item(it.ID)
	if got.State != StateWaitingBlair || v.openDecisions(it.ID) != 1 {
		t.Fatalf("state %s decisions %d, want waiting_blair with one decision", got.State, v.openDecisions(it.ID))
	}
	if v.checker.calls[songPath] != 0 {
		t.Fatal("an unsettled file must not be audio-checked as the match")
	}
}

func TestSweepItemChecksItsOwnTrackWithoutSearching(t *testing.T) {
	v := newEnv(t)
	v.mb.add(mbRec("rec-1", "Song", "Artist", "", "ISRC1"))
	recID := v.seedRecording("rec-1", "Song", "Artist", "ISRC1", "studio")
	res, err := v.db.Exec(`INSERT INTO mus_library_tracks (plex_rating_key, path, created_at) VALUES ('100', ?, 1)`, songPath)
	if err != nil {
		t.Fatal(err)
	}
	trackID, _ := res.LastInsertId()
	id := v.seedItem(KindSweep, StateLibraryCheck, recID)
	if _, err := v.db.Exec(`UPDATE mus_items SET track_id = ? WHERE id = ?`, trackID, id); err != nil {
		t.Fatal(err)
	}
	v.checker.results[songPath] = []audiocheck.Result{genuineFLAC()}
	v.tick(1)
	if got := v.item(id).State; got != StateDone {
		t.Fatalf("state %s", got)
	}
	if v.plex.searchCalls != 0 {
		t.Fatal("a sweep item with a known track must not search Plex")
	}
}

func TestIsLossless(t *testing.T) {
	for _, c := range []string{"flac", "alac", "pcm_s24le", "wavpack", "ape"} {
		if !isLossless(c) {
			t.Errorf("%s should be lossless", c)
		}
	}
	for _, c := range []string{"mp3", "aac", "opus", "vorbis", ""} {
		if isLossless(c) {
			t.Errorf("%s should be lossy", c)
		}
	}
}
