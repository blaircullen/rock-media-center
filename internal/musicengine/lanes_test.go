package musicengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tristenlammi/arrmada/internal/indexer"
	"github.com/tristenlammi/arrmada/internal/slskd"
)

// AE13: a rejected NZBgeek key marks usenet unavailable, and when torrents and Soulseek also
// miss, no "no genuine copy" decision is created from that lane's silence.
func TestLaneRejectedKeyIsUnavailableNotSearched(t *testing.T) {
	h := newAcqHarness(t)
	h.idx.failed = map[string]string{"NZBgeek": "401 API key rejected"}
	id := h.newItem(KindRequest, StateSearching)

	h.stepOK(id)

	it := h.item(id)
	if it.State != StateLaneUnavailable {
		t.Fatalf("state = %s (%s), want lane_unavailable", it.State, it.LastReason)
	}
	if h.openDecisions(id) != 0 || h.count(`SELECT COUNT(*) FROM mus_decisions`) != 0 {
		t.Fatal("a decision was created from an unavailable lane's silence")
	}
	if !strings.Contains(it.LastReason, "usenet") || !strings.Contains(it.LastReason, "API key rejected") {
		t.Errorf("reason = %q", it.LastReason)
	}
	var status, reason string
	var since int64
	if err := h.db.QueryRow(`SELECT status, reason, unavailable_since FROM mus_lane_status WHERE lane = 'usenet'`).Scan(&status, &reason, &since); err != nil {
		t.Fatal(err)
	}
	if status != "unavailable" || !strings.Contains(reason, "API key rejected") || since != h.clock.Now().Unix() {
		t.Errorf("usenet status = %s %q since %d", status, reason, since)
	}
	if it.NextAttemptAt <= h.clock.Now().Unix() {
		t.Error("lane_unavailable should back off before the next look")
	}

	// A second look while still down keeps the state and still raises nothing.
	h.stepOK(id)
	if st := h.item(id).State; st != StateLaneUnavailable || h.openDecisions(id) != 0 {
		t.Fatalf("second look: state %s decisions %d", st, h.openDecisions(id))
	}

	// The key is fixed and a release appears: lane_unavailable -> searching -> ... -> placing.
	h.idx.failed = map[string]string{}
	h.idx.byCat[3040] = []indexer.Release{h.usenetRelease(acqAlbum, 300<<20)}
	h.dl.rels[acqAlbum] = acqAlbumFiles()
	h.chk.set(acqTrack, acqGenuine())
	h.stepOK(id)
	if st := h.item(id).State; st != StatePlacing {
		t.Fatalf("after recovery: %s (%s)", st, h.item(id).LastReason)
	}
	if err := h.db.QueryRow(`SELECT status FROM mus_lane_status WHERE lane = 'usenet'`).Scan(&status); err != nil || status != "available" {
		t.Errorf("usenet status after recovery = %q, %v", status, err)
	}
}

// Only a lane's own indexers decide its availability: here every indexer fails, so every
// indexed lane is unavailable and the item waits instead of going to Blair.
func TestLaneAllIndexersFailedNeverBecomesADecision(t *testing.T) {
	h := newAcqHarness(t)
	h.idx.failed = map[string]string{"NZBgeek": "timeout", "TL": "login failed"}
	id := h.newItem(KindRequest, StateSearching)

	h.stepOK(id)

	if st := h.item(id).State; st != StateLaneUnavailable {
		t.Fatalf("state = %s", st)
	}
	for _, lane := range []string{LaneUsenet, LaneTorrent} {
		var status string
		if err := h.db.QueryRow(`SELECT status FROM mus_lane_status WHERE lane = ?`, lane).Scan(&status); err != nil || status != "unavailable" {
			t.Errorf("%s status = %q, %v", lane, status, err)
		}
	}
	var status string
	if err := h.db.QueryRow(`SELECT status FROM mus_lane_status WHERE lane = 'soulseek'`).Scan(&status); err != nil || status != "available" {
		t.Errorf("soulseek searched and found nothing: status = %q, %v", status, err)
	}
	if h.openDecisions(id) != 0 {
		t.Error("a decision was raised")
	}
}

// No enabled usenet indexer means the usenet lane did not look.
func TestLaneNoIndexerOfTransportIsUnavailable(t *testing.T) {
	h := newAcqHarness(t)
	h.idx.indexers = []indexer.Indexer{{Name: "TL", Kind: indexer.KindTorznab, Enabled: true}}
	id := h.newItem(KindRequest, StateSearching)

	h.stepOK(id)

	if st := h.item(id).State; st != StateLaneUnavailable {
		t.Fatalf("state = %s", st)
	}
	var reason string
	if err := h.db.QueryRow(`SELECT reason FROM mus_lane_status WHERE lane = 'usenet'`).Scan(&reason); err != nil || !strings.Contains(reason, "No enabled usenet indexer") {
		t.Fatalf("reason = %q, %v", reason, err)
	}
}

// KTD20: torrent and Soulseek are searched only while the VPN attachment holds; with it down
// (or never recorded) both are unavailable and neither client is touched.
func TestLaneVPNDownMakesTorrentAndSoulseekUnavailable(t *testing.T) {
	for _, tc := range []string{"check fails", "nothing recorded"} {
		t.Run(tc, func(t *testing.T) {
			h := newAcqHarness(t)
			h.slsk.files = []slskd.File{{Username: "bob", Filename: `@@m\Robyn\02 - Hang With Me.flac`, Extension: "flac", Size: 1}}
			h.idx.byCat[3040] = []indexer.Release{h.torrentRelease(acqAlbum, 20)}
			cfg := AcquireConfig{DB: h.db, Indexer: h.idx, Downloads: h.dl, Slskd: h.slsk, Checker: h.chk,
				Staging: h.staging, Now: h.clock.Now}
			if tc == "check fails" {
				cfg.VPN = func(context.Context) (bool, string) { return false, "kill switch is off" }
			}
			a, err := NewAcquire(cfg)
			if err != nil {
				t.Fatal(err)
			}
			h.acq = a
			id := h.newItem(KindRequest, StateSearching)

			h.stepOK(id)

			if st := h.item(id).State; st != StateLaneUnavailable {
				t.Fatalf("state = %s (%s)", st, h.item(id).LastReason)
			}
			if h.slsk.searches != 0 {
				t.Error("slskd was searched with the VPN unconfirmed")
			}
			if h.dl.addCount() != 0 {
				t.Error("a torrent was added with the VPN unconfirmed")
			}
			for _, lane := range []string{LaneTorrent, LaneSoulseek} {
				var status, reason string
				var vpn int
				if err := h.db.QueryRow(`SELECT status, reason, vpn_ok FROM mus_lane_status WHERE lane = ?`, lane).Scan(&status, &reason, &vpn); err != nil {
					t.Fatal(err)
				}
				if status != "unavailable" || vpn != 0 || !strings.Contains(reason, "The VPN isn't confirmed") {
					t.Errorf("%s = %s %q vpn=%d", lane, status, reason, vpn)
				}
			}
			if h.openDecisions(id) != 0 {
				t.Error("a decision was raised while lanes were unavailable")
			}
		})
	}
}

// Usenet misses, so torrents are searched; the torrent downloads straight into staging.
func TestLaneTorrentUsedWhenUsenetMisses(t *testing.T) {
	h := newAcqHarness(t)
	h.idx.byCat[3040] = []indexer.Release{h.torrentRelease(acqAlbum, 20), h.torrentRelease(acqAlbumB+" dead", 0)}
	h.dl.rels[acqAlbum] = acqAlbumFiles()
	h.chk.set(acqTrack, acqGenuine())
	id := h.newItem(KindRequest, StateSearching)

	h.stepOK(id)

	if st := h.item(id).State; st != StatePlacing {
		t.Fatalf("state = %s (%s)", st, h.item(id).LastReason)
	}
	cs := h.candidates(id)
	if len(cs) != 1 || cs[0].Lane != LaneTorrent {
		t.Fatalf("candidates = %+v, want one torrent candidate (the seederless one is dropped)", cs)
	}
	if h.slsk.searches != 0 {
		t.Error("soulseek searched although torrents produced a candidate")
	}
	if h.dl.adds[0].Transport != "torrent" || h.dl.adds[0].Category != "rmc-music" {
		t.Errorf("add = %+v", h.dl.adds[0])
	}
}

// Soulseek is the last lane: a single FLAC from a peer is downloaded, moved into staging
// and checked.
func TestLaneSoulseekFileDownloadedAndChecked(t *testing.T) {
	h := newAcqHarness(t)
	remote := `@@music\Robyn\Body Talk Pt. 2\02 - Hang With Me.flac`
	h.slsk.files = []slskd.File{
		{Username: "bob", Filename: remote, Extension: "flac", Size: 1, BitDepth: 16, FreeSlot: true},
		{Username: "eve", Filename: `@@music\Robyn\Hang With Me (Live).flac`, Extension: "flac", Size: 1},
		{Username: "zed", Filename: `@@music\Robyn\Hang With Me.mp3`, Extension: "mp3", Size: 1},
	}
	// slskd's finished file appears in its folder.
	done := filepath.Join(h.slsk.dir, "Body Talk Pt. 2")
	stagingWrite(t, filepath.Join(done, "02 - Hang With Me.flac"))
	h.chk.set(acqTrack, acqGenuine())
	id := h.newItem(KindRequest, StateSearching)

	h.stepOK(id)

	if st := h.item(id).State; st != StatePlacing {
		t.Fatalf("state = %s (%s)", st, h.item(id).LastReason)
	}
	// The live copy is a different recording and the mp3 is lossy: only bob's studio FLAC is queued.
	if len(h.slsk.enqueued) != 1 || !strings.HasPrefix(h.slsk.enqueued[0], "bob ") {
		t.Fatalf("enqueued = %v, want only bob's studio FLAC", h.slsk.enqueued)
	}
	var path string
	if err := h.db.QueryRow(`SELECT staging_path FROM mus_candidates WHERE state = 'chosen'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, h.staging.Root()) {
		t.Errorf("chosen file %s is not in staging", path)
	}
	if _, err := os.Stat(filepath.Join(done, "02 - Hang With Me.flac")); err == nil {
		t.Error("the finished file should have been moved out of slskd's folder")
	}
}

func TestLaneSoulseekErrorIsUnavailable(t *testing.T) {
	h := newAcqHarness(t)
	h.slsk.err = slskd.ErrUnavailable
	id := h.newItem(KindRequest, StateSearching)
	h.stepOK(id)
	if st := h.item(id).State; st != StateLaneUnavailable {
		t.Fatalf("state = %s", st)
	}
	var reason string
	_ = h.db.QueryRow(`SELECT reason FROM mus_lane_status WHERE lane = 'soulseek'`).Scan(&reason)
	if !strings.Contains(reason, "slskd") {
		t.Errorf("reason = %q", reason)
	}
}

func TestLaneNilClientsAreUnavailableNotSkipped(t *testing.T) {
	h := newAcqHarness(t)
	a, err := NewAcquire(AcquireConfig{DB: h.db, Checker: h.chk, Staging: h.staging, Now: h.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	h.acq = a
	id := h.newItem(KindRequest, StateSearching)
	h.stepOK(id)
	if st := h.item(id).State; st != StateLaneUnavailable {
		t.Fatalf("state = %s", st)
	}
	if n := h.count(`SELECT COUNT(*) FROM mus_lane_status WHERE status = 'unavailable'`); n != 3 {
		t.Errorf("unavailable lanes = %d, want 3", n)
	}
}

func TestLaneScoreRelease(t *testing.T) {
	q := laneQuery{Artist: "Robyn", Title: "Hang With Me", Album: "Body Talk Pt. 2"}
	rel := func(title string, size int64, seeders int, tr indexer.Transport) indexer.Release {
		return indexer.Release{Title: title, SizeBytes: size, Seeders: seeders, Transport: tr}
	}
	good, ok := laneScoreRelease(rel("Robyn - Body Talk Pt. 2 (2010) [FLAC]", 300<<20, 0, indexer.TransportUsenet), q, true)
	if !ok {
		t.Fatal("album FLAC should pass")
	}
	plain, ok := laneScoreRelease(rel("Robyn - Honey (2018) [FLAC]", 300<<20, 0, indexer.TransportUsenet), q, true)
	if !ok || plain >= good {
		t.Errorf("album-matching release must outscore another album: %d vs %d", good, plain)
	}
	for name, r := range map[string]indexer.Release{
		"lossy":       rel("Robyn - Body Talk Pt. 2 (2010) [MP3 320]", 100<<20, 0, indexer.TransportUsenet),
		"discography": rel("Robyn - Discography FLAC", 3<<30, 0, indexer.TransportUsenet),
		"oversized":   rel("Robyn - Body Talk Pt. 2 [FLAC]", 5<<30, 0, indexer.TransportUsenet),
		"other":       rel("Madonna - Ray of Light [FLAC]", 300<<20, 0, indexer.TransportUsenet),
		"no seeders":  rel("Robyn - Body Talk Pt. 2 [FLAC]", 300<<20, 0, indexer.TransportTorrent),
	} {
		if _, ok := laneScoreRelease(r, q, true); ok {
			t.Errorf("%s release passed pre-scoring", name)
		}
	}
	// A name with no stated quality passes only out of the lossless category.
	if _, ok := laneScoreRelease(rel("Robyn-Body_Talk_Pt_2-WEB-2010-GRP", 300<<20, 0, indexer.TransportUsenet), q, true); !ok {
		t.Error("unlabelled release from 3040 should pass")
	}
	if _, ok := laneScoreRelease(rel("Robyn-Body_Talk_Pt_2-WEB-2010-GRP", 300<<20, 0, indexer.TransportUsenet), q, false); ok {
		t.Error("unlabelled release from 3000 should not pass")
	}
}
