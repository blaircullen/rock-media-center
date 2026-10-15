package musicengine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tristenlammi/arrmada/internal/indexer"
	"github.com/tristenlammi/arrmada/internal/slskd"
)

// usenetItem seeds one usenet release holding the studio-named track and returns the item.
func (h *acqHarness) usenetItemFor(mbid, hint string) int64 {
	h.t.Helper()
	h.idx.byCat[3040] = []indexer.Release{h.usenetRelease(acqAlbum, 300<<20)}
	h.dl.rels[acqAlbum] = acqAlbumFiles()
	rec := h.seedRecording(mbid, "Hang With Me", "Robyn", "SEA011000001", hint)
	return h.seedItem(KindRequest, StateSearching, rec)
}

func (h *acqHarness) rejectReason(id int64) string {
	h.t.Helper()
	var r string
	if err := h.db.QueryRow(`SELECT reject_reason FROM mus_candidates WHERE item_id = ? AND state = 'rejected' ORDER BY id DESC LIMIT 1`, id).Scan(&r); err != nil {
		h.t.Fatalf("no rejected candidate: %v", err)
	}
	return r
}

// Finding 2: a spectral "Genuine" AAC/MP3 is not lossless and never reaches placement.
func TestAcquireRejectsLossyCodecThatReadsGenuine(t *testing.T) {
	for _, codec := range []string{"aac", "mp3", "vorbis", "unknown"} {
		t.Run(codec, func(t *testing.T) {
			h := newAcqHarness(t)
			id := h.newItem(KindRequest, StateSearching)
			h.idx.byCat[3040] = []indexer.Release{h.usenetRelease(acqAlbum, 300<<20)}
			h.dl.rels[acqAlbum] = acqAlbumFiles()
			r := acqGenuine()
			r.Facts.Codec = codec
			h.chk.set(acqTrack, r)

			h.stepOK(id)

			if st := h.item(id).State; st == StatePlacing {
				t.Fatalf("a %q file was sent to placement", codec)
			}
			if got := h.rejectReason(id); !strings.Contains(got, "not a lossless codec") {
				t.Errorf("reject_reason = %q", got)
			}
		})
	}
}

func TestAcquireAcceptsLosslessCodecs(t *testing.T) {
	for _, codec := range []string{"flac", "alac", "pcm_s24le", "wavpack"} {
		if !isLossless(codec) {
			t.Errorf("%s should be lossless", codec)
		}
	}
	for _, codec := range []string{"aac", "mp3", "opus", "vorbis", "", "wmav2"} {
		if isLossless(codec) {
			t.Errorf("%q should not be lossless", codec)
		}
	}
}

// Finding 4: the file's own recording id must match the pinned one.
func TestAcquireRejectsFileTaggedWithAnotherRecording(t *testing.T) {
	h := newAcqHarness(t)
	id := h.usenetItemFor("mbid-hwm", "studio")
	h.chk.set(acqTrack, acqGenuine())
	h.tags = map[string]AcquireFileTags{acqTrack: {RecordingMBID: "mbid-other"}}

	h.stepOK(id)

	if st := h.item(id).State; st == StatePlacing {
		t.Fatal("a file tagged as another recording was sent to placement")
	}
	if got := h.rejectReason(id); !strings.Contains(got, "wrong recording") {
		t.Errorf("reject_reason = %q", got)
	}
}

func TestAcquireAcceptsMatchingRecordingTagAndStoresReleaseIDsFromTheFile(t *testing.T) {
	h := newAcqHarness(t)
	id := h.usenetItemFor("mbid-hwm", "studio")
	// The pinned recording carries its own release ids; the candidate must not inherit them.
	if _, err := h.db.Exec(`UPDATE mus_recordings SET release_mbid = 'pinned-rel', release_group_mbid = 'pinned-rg'`); err != nil {
		t.Fatal(err)
	}
	h.chk.set(acqTrack, acqGenuine())
	h.tags = map[string]AcquireFileTags{acqTrack: {RecordingMBID: "MBID-HWM", ReleaseMBID: "file-rel", ReleaseGroupMBID: "file-rg"}}

	h.stepOK(id)

	if st := h.item(id).State; st != StatePlacing {
		t.Fatalf("state = %s (%s)", st, h.item(id).LastReason)
	}
	var rel, rg string
	if err := h.db.QueryRow(`SELECT release_mbid, release_group_mbid FROM mus_candidates WHERE state = 'chosen'`).Scan(&rel, &rg); err != nil {
		t.Fatal(err)
	}
	if rel != "file-rel" || rg != "file-rg" {
		t.Errorf("candidate release ids = %q %q, want the file's own", rel, rg)
	}
}

// Finding 11: a file with no release tags leaves the columns empty, never the pin's release.
func TestAcquireLeavesReleaseIDsEmptyWhenTheFileHasNone(t *testing.T) {
	h := newAcqHarness(t)
	id := h.usenetItemFor("mbid-hwm", "studio")
	if _, err := h.db.Exec(`UPDATE mus_recordings SET release_mbid = 'pinned-rel', release_group_mbid = 'pinned-rg'`); err != nil {
		t.Fatal(err)
	}
	h.chk.set(acqTrack, acqGenuine())

	h.stepOK(id)

	var rel, rg string
	if err := h.db.QueryRow(`SELECT release_mbid, release_group_mbid FROM mus_candidates WHERE state = 'chosen'`).Scan(&rel, &rg); err != nil {
		t.Fatal(err)
	}
	if rel != "" || rg != "" {
		t.Errorf("candidate release ids = %q %q, want empty", rel, rg)
	}
}

// A live pin needs "live" in the release or file name; the studio-named file is rejected.
func TestAcquireLivePinRequiresLiveInTheNames(t *testing.T) {
	h := newAcqHarness(t)
	id := h.usenetItemFor("mbid-hwm-live", "live")
	h.chk.set(acqTrack, acqGenuine())

	h.stepOK(id)

	if st := h.item(id).State; st == StatePlacing {
		t.Fatal("a studio-named file was placed for a live recording")
	}
	if got := h.rejectReason(id); !strings.Contains(got, "live version") {
		t.Errorf("reject_reason = %q", got)
	}
}

func TestAcquireRejectsLengthFarFromThePinnedRecording(t *testing.T) {
	h := newAcqHarness(t)
	id := h.usenetItemFor("mbid-hwm", "studio")
	r := acqGenuine()
	r.Facts.DurationSec = 290 // pinned length is 200 s
	h.chk.set(acqTrack, r)

	h.stepOK(id)

	if st := h.item(id).State; st == StatePlacing {
		t.Fatal("a 290 s file was placed for a 200 s recording")
	}
	if got := h.rejectReason(id); !strings.Contains(got, "wrong length") {
		t.Errorf("reject_reason = %q", got)
	}
}

func TestAcquireRejectsExplicitCleanMismatch(t *testing.T) {
	h := newAcqHarness(t)
	id := h.usenetItemFor("mbid-hwm", "studio") // the pin is not explicit
	h.chk.set(acqTrack, acqGenuine())
	h.tags = map[string]AcquireFileTags{acqTrack: {Advisory: "explicit"}}

	h.stepOK(id)

	if st := h.item(id).State; st == StatePlacing {
		t.Fatal("an explicit file was placed for the clean recording")
	}
	if got := h.rejectReason(id); !strings.Contains(got, "explicit") {
		t.Errorf("reject_reason = %q", got)
	}
}

// Ambiguity (no tags) keeps today's behaviour and says why in the item's reason.
func TestAcquireAmbiguousIdentityIsAcceptedWithTheReasonRecorded(t *testing.T) {
	h := newAcqHarness(t)
	id := h.usenetItemFor("mbid-hwm", "studio")
	h.chk.set(acqTrack, acqGenuine())

	h.stepOK(id)

	if st := h.item(id).State; st != StatePlacing {
		t.Fatalf("state = %s (%s)", st, h.item(id).LastReason)
	}
	if got := h.item(id).LastReason; !strings.Contains(got, "identity not proven") {
		t.Errorf("last_reason = %q, want the unproven identity recorded", got)
	}
}

// Finding 5: a candidate kept from an earlier pass is not started while the VPN is down.
func TestAcquireExistingTorrentAndSoulseekCandidatesWaitForTheVPN(t *testing.T) {
	cases := map[string]struct{ lane, src string }{
		"torrent":  {LaneTorrent, `{"indexer":"TL","url":"magnet:?xt=kept"}`},
		"soulseek": {LaneSoulseek, `{"user":"bob","filename":"@@m\\Robyn\\02 - Hang With Me.flac","size":1}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newAcqHarness(t)
			id := h.newItem(KindRequest, StateSearching)
			if _, err := h.db.Exec(
				`INSERT INTO mus_candidates (item_id, lane, state, release_name, source_url, score, created_at)
				 VALUES (?, ?, 'found', 'kept', ?, 10, ?)`, id, tc.lane, tc.src, h.clock.Now().Unix()); err != nil {
				t.Fatal(err)
			}
			h.vpnOK = false

			h.stepOK(id)

			if st := h.item(id).State; st != StateLaneUnavailable {
				t.Fatalf("state = %s (%s), want lane_unavailable", st, h.item(id).LastReason)
			}
			if !strings.Contains(h.item(id).LastReason, "VPN") {
				t.Errorf("last_reason = %q, want the VPN named", h.item(id).LastReason)
			}
			if h.dl.addCount() != 0 || len(h.slsk.enqueued) != 0 {
				t.Errorf("a client was touched with the VPN down: adds %d, enqueued %v", h.dl.addCount(), h.slsk.enqueued)
			}
			var state string
			_ = h.db.QueryRow(`SELECT state FROM mus_candidates WHERE item_id = ?`, id).Scan(&state)
			if state != "found" {
				t.Errorf("candidate state = %s, want it kept as found", state)
			}
		})
	}
}

// The same candidate starts once the VPN is back.
func TestAcquireDownloadingCandidateWithoutJobIsHeldWhenVPNDropsMidStart(t *testing.T) {
	h := newAcqHarness(t)
	id := h.newItem(KindRequest, StateDownloading)
	if _, err := h.db.Exec(
		`INSERT INTO mus_candidates (item_id, lane, state, release_name, source_url, score, created_at)
		 VALUES (?, 'torrent', 'downloading', 'kept', '{"indexer":"TL","url":"magnet:?xt=kept"}', 10, ?)`, id, h.clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	h.vpnOK = false

	h.stepOK(id)

	if st := h.item(id).State; st != StateLaneUnavailable {
		t.Fatalf("state = %s (%s)", st, h.item(id).LastReason)
	}
	if h.dl.addCount() != 0 {
		t.Error("a torrent was added with the VPN down")
	}
	var state string
	_ = h.db.QueryRow(`SELECT state FROM mus_candidates WHERE item_id = ?`, id).Scan(&state)
	if state != "found" {
		t.Errorf("candidate state = %s, want found", state)
	}
}

// Finding 8: a crash after the Soulseek file was moved into staging resumes without the
// original location.
func TestAcquireSoulseekResumesAfterCrashFollowingTheRename(t *testing.T) {
	h := newAcqHarness(t)
	remote := `@@music\Robyn\Body Talk Pt. 2\02 - Hang With Me.flac`
	h.slsk.files = []slskd.File{{Username: "bob", Filename: remote, Extension: "flac", Size: 1, BitDepth: 16, FreeSlot: true}}
	h.slsk.phase = slskd.PhasePending
	h.chk.set(acqTrack, acqGenuine())
	id := h.newItem(KindRequest, StateSearching)
	h.stepOK(id) // enqueued, still transferring
	if st := h.item(id).State; st != StateDownloading {
		t.Fatalf("state = %s (%s)", st, h.item(id).LastReason)
	}
	var dir string
	if err := h.db.QueryRow(`SELECT staging_path FROM mus_candidates WHERE item_id = ?`, id).Scan(&dir); err != nil {
		t.Fatal(err)
	}
	// The crashed attempt had recorded the destination and renamed; slskd's folder is empty.
	dst := filepath.Join(dir, acqTrack)
	stagingWrite(t, dst)
	if _, err := h.db.Exec(`UPDATE mus_candidates SET staging_path = ? WHERE item_id = ?`, dst, id); err != nil {
		t.Fatal(err)
	}
	h.slsk.phase = slskd.PhaseSucceeded

	h.acq = h.newAcquire()
	h.stepOK(id)

	if st := h.item(id).State; st != StatePlacing {
		t.Fatalf("state = %s (%s), want placing after resuming", st, h.item(id).LastReason)
	}
}

func TestAcquireAdoptIsIdempotentAndRecordsDestinationFirst(t *testing.T) {
	h := newAcqHarness(t)
	id := h.newItem(KindRequest, StateDownloading)
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "Release")
	stagingWrite(t, filepath.Join(src, "a.flac"))
	res, err := h.db.Exec(
		`INSERT INTO mus_candidates (item_id, lane, state, release_name, source_url, staging_path, score, created_at)
		 VALUES (?, 'usenet', 'downloading', 'r', '{}', ?, 1, 1)`, id, dir)
	if err != nil {
		t.Fatal(err)
	}
	cid, _ := res.LastInsertId()
	c := candRow{ID: cid, ItemID: id, StagingPath: dir}

	if err := h.acq.adopt(context.Background(), h.e, c, src, dir); err != nil {
		t.Fatal(err)
	}
	var got string
	_ = h.db.QueryRow(`SELECT staging_path FROM mus_candidates WHERE id = ?`, cid).Scan(&got)
	if want := filepath.Join(dir, "Release"); got != want {
		t.Errorf("staging_path = %q, want the destination %q", got, want)
	}
	if !stagingExists(filepath.Join(dir, "Release", "a.flac")) {
		t.Fatal("release was not moved")
	}
	// Retry after a crash: the source is gone, the destination is there.
	c.StagingPath = got
	if err := h.acq.adopt(context.Background(), h.e, c, src, dir); err != nil {
		t.Errorf("retry after the rename failed: %v", err)
	}
}

// Finding 4 (Soulseek): the finished file is matched on its folder as well as name and size,
// and two equal matches are an error, not the newest one.
func TestAcquireFindSoulseekFileIsBoundToTheTransfer(t *testing.T) {
	root := t.TempDir()
	remote := `@@a\Robyn\Body Talk Pt. 2\02 - Hang With Me.flac`
	stagingWrite(t, filepath.Join(root, "Body Talk Pt. 2", "02 - Hang With Me.flac"))
	stagingWrite(t, filepath.Join(root, "Greatest Hits", "02 - Hang With Me.flac")) // another peer, same name and size

	got, err := acqFindSoulseekFile(root, remote, 1)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(filepath.Dir(got)) != "Body Talk Pt. 2" {
		t.Errorf("picked %s, want the file from this transfer's folder", got)
	}

	stagingWrite(t, filepath.Join(root, "bob", "Body Talk Pt. 2", "02 - Hang With Me.flac"))
	if _, err := acqFindSoulseekFile(root, remote, 1); err == nil || !strings.Contains(err.Error(), "cannot tell which") {
		t.Errorf("two equal matches: err = %v, want an ambiguity error", err)
	}
}

func TestParseAcquireTagsReadsReleaseGroupAndAdvisory(t *testing.T) {
	j := `{"format":{"duration":"201.5","tags":{"MUSICBRAINZ_TRACKID":"11111111-1111-1111-1111-111111111111","MUSICBRAINZ_ALBUMID":"22222222-2222-2222-2222-222222222222","MUSICBRAINZ_RELEASEGROUPID":"33333333-3333-3333-3333-333333333333","ITUNESADVISORY":"1","title":"Hang With Me (Live)"}}}`
	got, err := parseAcquireTags([]byte(j))
	if err != nil {
		t.Fatal(err)
	}
	if got.RecordingMBID != "11111111-1111-1111-1111-111111111111" || got.ReleaseMBID != "22222222-2222-2222-2222-222222222222" ||
		got.ReleaseGroupMBID != "33333333-3333-3333-3333-333333333333" || got.Advisory != "explicit" || got.DurationSec != 201.5 {
		t.Errorf("tags = %+v", got)
	}
}
