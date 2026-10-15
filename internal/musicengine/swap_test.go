package musicengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tristenlammi/arrmada/internal/audiocheck"
	"github.com/tristenlammi/arrmada/internal/plex"
)

// ---- the swap rig ----
//
// Files are tiny text blobs whose "audio" part carries markers the fake checker reads:
//
//	B24 bit depth   R96000 sample rate   T16000 spectral top Hz   L-9.5 loudness LUFS
//	D200000 length ms   FAKE / SUSPECT / CORRUPT verdicts   LOSSY lossy codec
//
// The audio string is also the PCM fingerprint, so every version carries a distinct label.

var swapMarkRe = regexp.MustCompile(`\b([BRTLD])(-?[0-9]+(?:\.[0-9]+)?)\b`)

type swapAudio struct {
	Bits, Rate, Top int
	LUFS            float64
	LenMs           int64
	Verdict         audiocheck.Verdict
	Lossy           bool
}

func swapParseAudio(audio string) swapAudio {
	a := swapAudio{Bits: 16, Rate: 44100, Top: 21000, LUFS: -9.0, LenMs: 200000, Verdict: audiocheck.Genuine}
	topSet := false
	for _, m := range swapMarkRe.FindAllStringSubmatch(audio, -1) {
		v, _ := strconv.ParseFloat(m[2], 64)
		switch m[1] {
		case "B":
			a.Bits = int(v)
		case "R":
			a.Rate = int(v)
		case "T":
			a.Top, topSet = int(v), true
		case "L":
			a.LUFS = v
		case "D":
			a.LenMs = int64(v)
		}
	}
	switch {
	case strings.Contains(audio, "FAKE"):
		a.Verdict = audiocheck.Fake
		if !topSet {
			a.Top = 16000
		}
	case strings.Contains(audio, "SUSPECT"):
		a.Verdict = audiocheck.Suspect
		if !topSet {
			a.Top = 19000
		}
	case strings.Contains(audio, "CORRUPT"):
		a.Verdict = audiocheck.Corrupt
	}
	if strings.Contains(audio, "LOSSY") {
		a.Lossy, a.Bits = true, 0
	}
	return a
}

type swapChecker struct {
	blind map[string]int // path -> reads that come back blind
	calls map[string]int
}

func (c *swapChecker) Check(_ context.Context, path string) audiocheck.Result {
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[path]++
	if c.blind[path] > 0 {
		c.blind[path]--
		return audiocheck.Result{Path: path, Verdict: audiocheck.Blind, Reason: "NFS hiccup"}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return audiocheck.Result{Path: path, Verdict: audiocheck.Blind, Reason: err.Error()}
	}
	audio, _, _ := placeDecode(b)
	a := swapParseAudio(audio)
	lufs := a.LUFS
	r := audiocheck.Result{
		Path: path, Verdict: a.Verdict, Reason: "test", Fingerprint: placeFP(audio),
		Facts: audiocheck.Facts{Codec: "flac", Container: "flac", BitDepth: a.Bits, SampleRate: a.Rate, Channels: 2,
			BitrateKbps: 900, DurationSec: float64(a.LenMs) / 1000},
		Integrity:    audiocheck.Integrity{OK: a.Verdict != audiocheck.Corrupt, DecodedSec: float64(a.LenMs) / 1000},
		Signature:    &audiocheck.Signature{TopHz: float64(a.Top), FloorDB: -70},
		LoudnessLUFS: &lufs,
	}
	if a.Lossy {
		r.Facts.Codec, r.Facts.Container, r.Facts.BitrateKbps = "aac", "m4a", 256
	}
	return r
}

// swapFakePlex adds what the swap needs on top of the placement fake: sessions, and a hook
// that runs when a folder is refreshed (to model Plex re-creating a track).
type swapFakePlex struct {
	*placeFakePlex
	playing   map[string]bool
	sessCalls int
	// playFrom, when > 0, makes every Sessions call from that call number on report the
	// track in playFromKey as playing.
	playFrom    int
	playFromKey string
	sessErr     error
	onRefresh   func()
	// hide makes searches list nothing, as when Plex has not indexed a folder yet.
	hide bool
}

func (f *swapFakePlex) SearchTracks(ctx context.Context, s, artist, title string) ([]plex.Track, error) {
	if f.hide {
		return nil, nil
	}
	return f.placeFakePlex.SearchTracks(ctx, s, artist, title)
}

func (f *swapFakePlex) Sessions(context.Context) ([]plex.Session, error) {
	f.sessCalls++
	if f.sessErr != nil {
		return nil, f.sessErr
	}
	var out []plex.Session
	for k, on := range f.playing {
		if on {
			out = append(out, plex.Session{RatingKey: k, Type: "track"})
		}
	}
	if f.playFrom > 0 && f.sessCalls >= f.playFrom {
		out = append(out, plex.Session{RatingKey: f.playFromKey, Type: "track"})
	}
	return out, nil
}

func (f *swapFakePlex) RefreshPath(ctx context.Context, s, dir string) error {
	if err := f.placeFakePlex.RefreshPath(ctx, s, dir); err != nil {
		return err
	}
	if f.onRefresh != nil {
		f.onRefresh()
	}
	return nil
}

type swapHarness struct {
	t    *testing.T
	pr   *placeRig
	v    *env
	fp   *swapFakePlex
	chk  *swapChecker
	q    *Quarantine
	sw   *Swaps
	pl   *Placement
	auto bool
	n    int // counter for unique names
}

func newSwapHarness(t *testing.T) *swapHarness {
	t.Helper()
	pr := newPlaceRig(t)
	r := &swapHarness{t: t, pr: pr, v: pr.v, auto: true, chk: &swapChecker{blind: map[string]int{}}}
	r.fp = &swapFakePlex{placeFakePlex: pr.plex, playing: map[string]bool{}}
	r.build()
	return r
}

// build makes a fresh Quarantine, Swaps and Placement over the same database and disk, as a
// process restart would.
func (r *swapHarness) build() {
	r.t.Helper()
	var err error
	r.q, err = NewQuarantine(QuarantineConfig{DB: r.v.db, Root: r.pr.quar, Now: r.v.clock.Now})
	if err != nil {
		r.t.Fatal(err)
	}
	r.sw, err = NewSwaps(SwapsConfig{
		Checker: r.chk, Plex: r.fp, Quarantine: r.q, AutoSwaps: func() bool { return r.auto },
		Now: r.v.clock.Now,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	r.pl, err = NewPlacement(PlacementConfig{
		LibraryRoot: r.pr.lib, PlexRoot: r.pr.plexRt, QuarantineRoot: r.pr.quar,
		PlexSection: func() string { return "5" },
		Plex:        r.fp, Checker: r.chk, Tags: placeFakeTagReader{}, Editor: r.pr.ed,
		Chown:   func(string, int, int) error { return nil },
		Euid:    func() int { return 1000 },
		Measure: func(string) bool { return true },
		Sleep:   func(context.Context, time.Duration) {},
		Swaps:   r.sw, Finalize: r.sw.Finalize,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	r.pl.Bind(r.v.e)
	if err := r.sw.Bind(r.pl); err != nil {
		r.t.Fatal(err)
	}
}

type swapTrackOpts struct {
	Title, Artist, Album string
	Audio                string
	Ext                  string // ".flac" by default
	RG                   string
	Rating               float64
	Playlists            []string
	PlayCount            int
	Explicit             bool
}

type swapInc struct {
	TrackID, RecID int64
	Local, PlexP   string
	Key            string
	MBID           string
}

func (o *swapTrackOpts) defaults(n int) {
	if o.Title == "" {
		o.Title = fmt.Sprintf("Song %d", n)
	}
	if o.Artist == "" {
		o.Artist = "Band"
	}
	if o.Album == "" {
		o.Album = "Album"
	}
	if o.Ext == "" {
		o.Ext = ".flac"
	}
	if o.Audio == "" {
		o.Audio = "INCUMBENT-" + o.Title
	}
}

// seedTrack puts a library file on disk, the Plex fake, and the catalogue.
func (r *swapHarness) seedTrack(o swapTrackOpts) swapInc {
	r.t.Helper()
	r.n++
	o.defaults(r.n)
	mbid := "mbid-" + strings.ReplaceAll(o.Title, " ", "-")
	local := filepath.Join(r.pr.lib, o.Artist, o.Album, "01 - "+o.Title+o.Ext)
	placeWrite(r.t, local, o.Audio, TrackTags{Artist: o.Artist, Album: o.Album, Title: o.Title, Track: 1, RecordingMBID: mbid})
	plexP := r.fp.toPlex(local)
	r.fp.refreshed[filepath.Dir(plexP)] = true
	key := r.fp.keyFor(plexP)
	if o.Rating > 0 {
		r.fp.ratings[key] = o.Rating
	}
	for _, pt := range o.Playlists {
		found := ""
		for k, p := range r.fp.playlists {
			if p.Title == pt {
				found = k
			}
		}
		if found == "" {
			if _, err := r.fp.CreatePlaylist(context.Background(), pt, []string{key}); err != nil {
				r.t.Fatal(err)
			}
		} else if err := r.fp.AddToPlaylist(context.Background(), found, []string{key}); err != nil {
			r.t.Fatal(err)
		}
	}
	recID := r.v.seedRecording(mbid, o.Title, o.Artist, "", "studio")
	if o.Explicit {
		r.exec(`UPDATE mus_recordings SET explicit = 1 WHERE id = ?`, recID)
	}
	now := r.v.clock.Now().Unix()
	var rating any
	if o.Rating > 0 {
		rating = o.Rating
	}
	res, err := r.v.db.Exec(
		`INSERT INTO mus_library_tracks (plex_rating_key, path, recording_id, title, artist, album, release_group_mbid,
		        explicit, user_rating, play_count, playlist_count, last_seen_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		key, plexP, recID, o.Title, o.Artist, o.Album, o.RG, boolInt(o.Explicit), rating, o.PlayCount, len(o.Playlists), now, now)
	if err != nil {
		r.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return swapInc{TrackID: id, RecID: recID, Local: local, PlexP: plexP, Key: key, MBID: mbid}
}

func (r *swapHarness) exec(q string, args ...any) {
	r.t.Helper()
	if _, err := r.v.db.Exec(q, args...); err != nil {
		r.t.Fatalf("%s: %v", q, err)
	}
}

type swapCandOpts struct {
	Audio       string
	Ext         string
	RG          string
	ReleaseName string
	NoRG        bool // leave the release group empty
	NoLoudness  bool
}

// seedSwap creates a sweep item in swapping for the track, with a chosen, staged candidate.
func (r *swapHarness) seedSwap(inc swapInc, c swapCandOpts) (itemID, candID int64) {
	r.t.Helper()
	r.n++
	if c.Ext == "" {
		c.Ext = ".flac"
	}
	if c.Audio == "" {
		c.Audio = fmt.Sprintf("CANDIDATE-%d", r.n)
	}
	if c.RG == "" && !c.NoRG {
		c.RG = "rg1"
	}
	itemID = r.v.seedItem(KindSweep, StateSwapping, inc.RecID)
	r.exec(`UPDATE mus_items SET track_id = ? WHERE id = ?`, inc.TrackID, itemID)
	var title, artist, album string
	if err := r.v.db.QueryRow(`SELECT title, artist, album FROM mus_library_tracks WHERE id = ?`, inc.TrackID).Scan(&title, &artist, &album); err != nil {
		r.t.Fatal(err)
	}
	res, err := r.v.db.Exec(`INSERT INTO mus_candidates (item_id, lane, state, release_name, release_mbid, created_at) VALUES (?, 'usenet', 'chosen', ?, 'rel-x', ?)`,
		itemID, c.ReleaseName, r.v.clock.Now().Unix())
	if err != nil {
		r.t.Fatal(err)
	}
	candID, _ = res.LastInsertId()
	src := filepath.Join(r.pr.stage, fmt.Sprint(itemID), fmt.Sprint(candID), "01 - "+title+c.Ext)
	sha := placeWrite(r.t, src, c.Audio, TrackTags{Artist: artist, Album: album, Title: title, Track: 1, RecordingMBID: inc.MBID})
	a := swapParseAudio(c.Audio)
	verdict, codec, container, bitrate := string(a.Verdict), "flac", "flac", 900
	if a.Lossy {
		verdict, codec, container, bitrate = "LOSSY", "aac", "m4a", 256
	}
	var lufs any = a.LUFS
	if c.NoLoudness {
		lufs = nil
	}
	r.exec(`UPDATE mus_candidates SET staging_path = ?, sha256 = ?, pcm_fingerprint = ?, verdict = ?, integrity_ok = 1,
	        codec = ?, container = ?, bit_depth = ?, sample_rate = ?, channels = 2, bitrate = ?, duration_ms = ?, decoded_ms = ?,
	        loudness_lufs = ?, spectral_top_hz = ?, release_group_mbid = ? WHERE id = ?`,
		src, sha, placeFP(c.Audio), verdict, codec, container, a.Bits, a.Rate, bitrate, a.LenMs, a.LenMs, lufs, a.Top, c.RG, candID)
	return itemID, candID
}

func (r *swapHarness) step(id int64) error {
	r.t.Helper()
	return r.pl.Step(context.Background(), r.v.e, r.v.item(id))
}

func (r *swapHarness) mustStep(id int64) {
	r.t.Helper()
	if err := r.step(id); err != nil {
		r.t.Fatalf("step of item %d in %s: %v", id, r.v.item(id).State, err)
	}
}

// drive steps the item until it is in one of the wanted states.
func (r *swapHarness) drive(id int64, n int, want ...State) {
	r.t.Helper()
	for i := 0; i < n; i++ {
		st := r.v.item(id).State
		for _, w := range want {
			if st == w {
				return
			}
		}
		r.mustStep(id)
	}
	it := r.v.item(id)
	for _, w := range want {
		if it.State == w {
			return
		}
	}
	r.t.Fatalf("item %d is %s after %d steps (reason %q), wanted one of %v", id, it.State, n, it.LastReason, want)
}

func (r *swapHarness) sha(path string) string {
	r.t.Helper()
	s, err := placeSHA256(path)
	if err != nil {
		r.t.Fatal(err)
	}
	return s
}

func (r *swapHarness) audioOf(path string) string {
	r.t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		r.t.Fatal(err)
	}
	a, _, err := placeDecode(b)
	if err != nil {
		r.t.Fatal(err)
	}
	return a
}

func (r *swapHarness) journalRows(kind string) int {
	return r.v.count(`SELECT COUNT(*) FROM mus_swap_journal WHERE kind = ?`, kind)
}

func (r *swapHarness) openDecision(itemID int64, kind string) Decision {
	r.t.Helper()
	rows, err := r.v.db.Query(`SELECT `+decCols+` FROM mus_decisions WHERE item_id = ? AND kind = ? AND answered_at = 0`, itemID, kind)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		r.t.Fatalf("item %d has no open %s decision", itemID, kind)
	}
	d, err := decScan(rows)
	if err != nil {
		r.t.Fatal(err)
	}
	return d
}

func (r *swapHarness) ratingOf(key string) float64 {
	r.fp.mu.Lock()
	defer r.fp.mu.Unlock()
	return r.fp.ratings[key]
}

func (r *swapHarness) inPlaylist(title, key string) bool {
	r.fp.mu.Lock()
	defer r.fp.mu.Unlock()
	for _, p := range r.fp.playlists {
		if p.Title != title {
			continue
		}
		for _, k := range p.Keys {
			if k == key {
				return true
			}
		}
	}
	return false
}

func (r *swapHarness) keyOf(trackID int64) string {
	var k string
	if err := r.v.db.QueryRow(`SELECT plex_rating_key FROM mus_library_tracks WHERE id = ?`, trackID).Scan(&k); err != nil {
		r.t.Fatal(err)
	}
	return k
}

func (r *swapHarness) nullTime(q string, args ...any) sql.NullInt64 {
	var n sql.NullInt64
	if err := r.v.db.QueryRow(q, args...).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

// ---- tests ----

// AE3: a SUSPECT library copy and a passing candidate of the same recording and mastering swap
// at the same path, through the real Placement, journal and Plex confirmation.
func TestSwapSuspectIncumbentReplacedAtSamePath(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Find Me", Artist: "Kings of Leon", Audio: "OLD-SUSPECT SUSPECT", RG: "rg1"})
	item, _ := r.seedSwap(inc, swapCandOpts{Audio: "NEW-GENUINE-FLAC"})
	oldSHA := r.sha(inc.Local)

	r.drive(item, 6, StateDone)

	if r.audioOf(inc.Local) != "NEW-GENUINE-FLAC" {
		t.Fatalf("library file holds %q, want the candidate", r.audioOf(inc.Local))
	}
	if got := r.sha(inc.Local); got == oldSHA {
		t.Fatal("library file is unchanged")
	}
	if got := placeTemps(t, r.pr.lib); len(got) != 0 {
		t.Errorf("temp files left: %v", got)
	}
	// Exactly one library file in the folder: the old name was replaced, not added alongside.
	if flacs := r.pr.libFlacs(); len(flacs) != 1 || flacs[0] != inc.Local {
		t.Errorf("library flacs = %v, want only %s", flacs, inc.Local)
	}
	// The replaced version is in quarantine with a row.
	var qpath, qsha string
	var qtrack, size int64
	if err := r.v.db.QueryRow(`SELECT path, sha256, track_id, size_bytes FROM mus_quarantine`).Scan(&qpath, &qsha, &qtrack, &size); err != nil {
		t.Fatalf("quarantine row: %v", err)
	}
	if qsha != oldSHA || qtrack != inc.TrackID || size == 0 || r.sha(qpath) != oldSHA {
		t.Errorf("quarantine row sha %s track %d size %d file sha %s, want the old version %s of track %d", qsha, qtrack, size, r.sha(qpath), oldSHA, inc.TrackID)
	}
	if !strings.HasPrefix(qpath, r.pr.quar+string(filepath.Separator)+fmt.Sprint(inc.TrackID)+string(filepath.Separator)) {
		t.Errorf("quarantine path %s is not under the track's own folder", qpath)
	}
	// The journal row is a closed swap, with the candidate's checksum published.
	var kind, published, incumbent string
	var phase int
	var closed int64
	if err := r.v.db.QueryRow(`SELECT kind, phase, published_sha256, incumbent_sha256, closed_at FROM mus_swap_journal`).Scan(&kind, &phase, &published, &incumbent, &closed); err != nil {
		t.Fatal(err)
	}
	if kind != JournalSwap || phase != PhasePlexConfirmed || closed == 0 || incumbent != oldSHA || published != r.sha(inc.Local) {
		t.Errorf("journal row kind %s phase %d closed %d incumbent %s published %s", kind, phase, closed, incumbent, published)
	}
	// The library track now carries the new file's facts and the engine's identity record.
	var verdict, fp string
	if err := r.v.db.QueryRow(`SELECT verdict, pcm_fingerprint FROM mus_library_tracks WHERE id = ?`, inc.TrackID).Scan(&verdict, &fp); err != nil {
		t.Fatal(err)
	}
	if verdict != "GENUINE" || fp != placeFP("NEW-GENUINE-FLAC") {
		t.Errorf("library track verdict %q fingerprint %q, want GENUINE and the candidate's", verdict, fp)
	}
	// The daily digest lists the swap (R25).
	d, err := NewDigest(r.v.db, r.v.notifier, r.v.clock.Now).Build(context.Background(), r.v.clock.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Swaps) != 1 {
		t.Errorf("digest lists %d swaps, want 1", len(d.Swaps))
	}
	// The track's sweep schedule starts over after a swap.
	if n := r.nullTime(`SELECT retry_interval_s FROM mus_library_tracks WHERE id = ?`, inc.TrackID); n.Int64 != 0 {
		t.Errorf("retry interval after a swap = %d, want 0", n.Int64)
	}
}

// Rating and playlist memberships read back unchanged after a swap (R24, KTD8): Plex keeps
// the track item, and the recorded state is on the journal row and in the identity tables.
func TestSwapRatingAndMembershipsReadBackUnchanged(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "OLD-GOLD SUSPECT", RG: "rg1", Rating: 8, Playlists: []string{"Road trip", "Songs" + MirrorSuffix}})
	item, _ := r.seedSwap(inc, swapCandOpts{Audio: "NEW-GOLD"})

	r.drive(item, 6, StateDone)

	key := r.keyOf(inc.TrackID)
	if key != inc.Key {
		t.Fatalf("rating key changed %s -> %s although Plex kept the item", inc.Key, key)
	}
	if got := r.ratingOf(key); got != 8 {
		t.Errorf("rating after the swap = %v, want 8", got)
	}
	for _, p := range []string{"Road trip", "Songs" + MirrorSuffix} {
		if !r.inPlaylist(p, key) {
			t.Errorf("track left playlist %q", p)
		}
	}
	var rb sql.NullFloat64
	var members string
	if err := r.v.db.QueryRow(`SELECT rating_before, memberships_before FROM mus_swap_journal WHERE kind = 'swap'`).Scan(&rb, &members); err != nil {
		t.Fatal(err)
	}
	if !rb.Valid || rb.Float64 != 8 || !strings.Contains(members, "Road trip") || !strings.Contains(members, `"mirror":true`) {
		t.Errorf("journal recorded rating %v memberships %q", rb, members)
	}
	if n := r.v.count(`SELECT COUNT(*) FROM mus_track_memberships m JOIN mus_track_identities i ON i.id = m.identity_id WHERE i.recording_id = ?`, inc.RecID); n != 2 {
		t.Errorf("identity memberships recorded = %d, want 2", n)
	}
	var idRating sql.NullFloat64
	if err := r.v.db.QueryRow(`SELECT rating FROM mus_track_identities WHERE recording_id = ?`, inc.RecID).Scan(&idRating); err != nil || !idRating.Valid || idRating.Float64 != 8 {
		t.Errorf("identity rating = %v (%v), want 8", idRating, err)
	}
}

// When Plex re-creates the track on refresh (new rating key, rating and playlist entries
// lost), Finalize puts both back and reads them back.
func TestSwapFinalizeRestoresRatingAndMembershipsOnANewRatingKey(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "OLD-GOLD SUSPECT", RG: "rg1", Rating: 10, Playlists: []string{"Road trip"}})
	item, _ := r.seedSwap(inc, swapCandOpts{Audio: "NEW-GOLD"})
	r.fp.onRefresh = func() {
		r.fp.salt += "z" // Plex re-created the item: new key, no rating
		r.fp.ratings = map[string]float64{}
		r.fp.onRefresh = nil
	}

	r.drive(item, 6, StateDone)

	key := r.keyOf(inc.TrackID)
	if key == inc.Key {
		t.Fatal("test setup: the rating key should have changed")
	}
	if got := r.ratingOf(key); got != 10 {
		t.Errorf("rating on the new key = %v, want 10 re-applied", got)
	}
	if !r.inPlaylist("Road trip", key) {
		t.Error("the new track is not in the playlist the old one was in")
	}
}

// Rating that Plex did not lose, and that Blair changed meanwhile, is left alone.
func TestSwapFinalizeNeverOverwritesALiveRating(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "OLD-GOLD SUSPECT", RG: "rg1", Rating: 4})
	item, _ := r.seedSwap(inc, swapCandOpts{Audio: "NEW-GOLD"})
	r.drive(item, 1, StateAwaitingPlexIndex, StateDone) // swap published
	r.fp.mu.Lock()
	r.fp.ratings[inc.Key] = 10 // Blair re-rated while the swap finished
	r.fp.mu.Unlock()
	r.drive(item, 4, StateDone)
	if got := r.ratingOf(inc.Key); got != 10 {
		t.Errorf("rating = %v, want Blair's newer 10 kept", got)
	}
}

// A track that is playing is not swapped until playback stops (R24).
func TestSwapWaitsWhileTheTrackIsPlaying(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "OLD-GOLD SUSPECT", RG: "rg1"})
	item, _ := r.seedSwap(inc, swapCandOpts{Audio: "NEW-GOLD"})
	old := r.sha(inc.Local)
	r.fp.playing[inc.Key] = true

	r.mustStep(item)
	if st := r.v.item(item).State; st != StateWaitingForPlayback {
		t.Fatalf("state = %s, want waiting_for_playback", st)
	}
	for i := 0; i < 3; i++ {
		r.mustStep(item)
	}
	if r.sha(inc.Local) != old || r.journalRows(JournalSwap) != 0 {
		t.Fatal("the library file or journal changed while the track was playing")
	}
	if st := r.v.item(item).State; st != StateWaitingForPlayback {
		t.Fatalf("state = %s while still playing, want waiting_for_playback", st)
	}

	r.fp.playing[inc.Key] = false
	r.mustStep(item)
	if st := r.v.item(item).State; st != StateSwapping {
		t.Fatalf("state = %s after playback stopped, want swapping", st)
	}
	r.drive(item, 6, StateDone)
	if r.audioOf(inc.Local) != "NEW-GOLD" {
		t.Error("the swap did not happen after playback stopped")
	}
}

// Playback that starts while the candidate is being staged stops the swap before the file is
// touched; the verified temp file waits and the swap finishes when playback ends.
func TestSwapPlaybackStartingDuringStagingHoldsTheRename(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "OLD-GOLD SUSPECT", RG: "rg1"})
	item, _ := r.seedSwap(inc, swapCandOpts{Audio: "NEW-GOLD"})
	old := r.sha(inc.Local)
	r.fp.playFrom, r.fp.playFromKey = 2, inc.Key // the second look at Plex finds it playing

	r.mustStep(item)
	if st := r.v.item(item).State; st != StateWaitingForPlayback {
		t.Fatalf("state = %s, want waiting_for_playback", st)
	}
	if r.sha(inc.Local) != old {
		t.Fatal("the library file changed while the track was playing")
	}
	var phase int
	if err := r.v.db.QueryRow(`SELECT phase FROM mus_swap_journal`).Scan(&phase); err != nil || phase != PhaseTempVerified {
		t.Fatalf("journal phase = %d (%v), want temp_verified held", phase, err)
	}
	r.fp.playFrom = 0
	r.drive(item, 8, StateDone)
	if r.audioOf(inc.Local) != "NEW-GOLD" {
		t.Error("the held swap did not finish")
	}
}

// After PlaybackWait the swap gives the track back to the next sweep cycle.
func TestSwapPlaybackWaitIsBounded(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "OLD-GOLD SUSPECT", RG: "rg1"})
	item, _ := r.seedSwap(inc, swapCandOpts{Audio: "NEW-GOLD"})
	r.exec(`UPDATE mus_library_tracks SET retry_interval_s = 1209600 WHERE id = ?`, inc.TrackID)
	r.fp.playing[inc.Key] = true
	r.mustStep(item)
	r.v.clock.Advance(DefaultPlaybackWait + time.Minute)
	r.mustStep(item)
	it := r.v.item(item)
	if it.State != StateDropped || !strings.Contains(it.LastReason, "next scan") {
		t.Fatalf("item is %s (%q), want dropped with the deferral reason", it.State, it.LastReason)
	}
	var next, interval int64
	if err := r.v.db.QueryRow(`SELECT next_sweep_at, retry_interval_s FROM mus_library_tracks WHERE id = ?`, inc.TrackID).Scan(&next, &interval); err != nil {
		t.Fatal(err)
	}
	if want := r.v.clock.Now().Add(sweepDeferAfter).Unix(); next != want {
		t.Errorf("next sweep = %d, want %d (a day away)", next, want)
	}
	if interval != 1209600 {
		t.Errorf("retry interval = %d, want it unchanged (a deferral is not a miss)", interval)
	}
	if r.audioOf(inc.Local) != "OLD-GOLD SUSPECT" {
		t.Error("the library file changed")
	}
}

// A Plex that cannot say who is playing stops the swap with an error; it never counts as idle.
func TestSwapPlexSessionErrorStopsTheSwap(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "OLD-GOLD SUSPECT", RG: "rg1"})
	item, _ := r.seedSwap(inc, swapCandOpts{Audio: "NEW-GOLD"})
	r.fp.sessErr = errors.New("plex 503")
	if err := r.step(item); err == nil || !strings.Contains(err.Error(), "plex 503") {
		t.Fatalf("step err = %v, want the Plex error surfaced", err)
	}
	if r.audioOf(inc.Local) != "OLD-GOLD SUSPECT" || r.journalRows(JournalSwap) != 0 {
		t.Fatal("the swap went ahead without knowing whether the track was playing")
	}
}

// AE6: a library file that reads BLIND is rechecked and not replaced; once it reads genuine
// 24-bit, a 16-bit candidate is not an upgrade.
func TestSwapBlindIncumbentIsRecheckedAndNotReplacedByA16Bit(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "HIRES-OLD B24 R96000", RG: "rg1"})
	item, cand := r.seedSwap(inc, swapCandOpts{Audio: "SIXTEEN-BIT B16"})
	old := r.sha(inc.Local)
	r.chk.blind[inc.Local] = 1

	if err := r.step(item); err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("first step err = %v, want a retry because the file read blind", err)
	}
	if r.sha(inc.Local) != old || r.journalRows(JournalSwap) != 0 {
		t.Fatal("a blind read changed the library")
	}
	r.mustStep(item) // the recheck reads genuine 24-bit
	it := r.v.item(item)
	if it.State != StateDropped {
		t.Fatalf("item is %s (%q), want dropped: the 16-bit candidate is not an upgrade", it.State, it.LastReason)
	}
	if r.sha(inc.Local) != old || r.journalRows(JournalSwap) != 0 {
		t.Fatal("the 24-bit file was replaced")
	}
	var st string
	_ = r.v.db.QueryRow(`SELECT state FROM mus_candidates WHERE id = ?`, cand).Scan(&st)
	if st != "rejected" {
		t.Errorf("candidate state = %s, want rejected", st)
	}
	if n := r.nullTime(`SELECT retry_interval_s FROM mus_library_tracks WHERE id = ?`, inc.TrackID); n.Int64 != int64(DefaultSweepBase/time.Second) {
		t.Errorf("a miss set the retry interval to %d, want the base", n.Int64)
	}
}

// A file that stays unreadable goes to Blair as a recovery decision, never to the upgrade path (R14).
func TestSwapStaysBlindEscalatesToADecisionAndNeverReplaces(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "HIRES-OLD B24", RG: "rg1"})
	item, _ := r.seedSwap(inc, swapCandOpts{Audio: "SIXTEEN-BIT"})
	old := r.sha(inc.Local)
	r.chk.blind[inc.Local] = 100
	for i := 0; i < maxBlindAttempts+2 && r.v.item(item).State == StateSwapping; i++ {
		// the engine's Tick records the failed attempt; do the same bookkeeping here
		if err := r.step(item); err != nil {
			r.exec(`UPDATE mus_items SET attempts = attempts + 1 WHERE id = ?`, item)
		}
	}
	if st := r.v.item(item).State; st != StateWaitingBlair {
		t.Fatalf("item is %s, want waiting_blair after %d unreadable reads", st, maxBlindAttempts)
	}
	r.openDecision(item, DecisionBlindIncumbent)
	if r.sha(inc.Local) != old || r.journalRows(JournalSwap) != 0 {
		t.Fatal("the unreadable file was replaced")
	}
}

// AE7: 16-bit/96 kHz incumbent against a 24-bit/44.1 kHz candidate is a trade-off, put to
// Blair; approving it swaps, through the real answer path.
func TestSwapTradeOffGoesToBlairThenApproveSwaps(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "OLD-A B16 R96000", RG: "rg1"})
	item, cand := r.seedSwap(inc, swapCandOpts{Audio: "NEW-B B24 R44100"})
	old := r.sha(inc.Local)

	r.mustStep(item)
	it := r.v.item(item)
	if it.State != StateWaitingBlair {
		t.Fatalf("item is %s (%q), want waiting_blair", it.State, it.LastReason)
	}
	d := r.openDecision(item, DecisionTradeOff)
	if d.CandidateID != cand {
		t.Errorf("decision names candidate %d, want %d", d.CandidateID, cand)
	}
	if !decHasOption(d.Options, OptionApprove) || !decHasOption(d.Options, OptionKeep) {
		t.Errorf("options = %v, want approve and keep", d.Options)
	}
	if !strings.Contains(d.Reason, "bit depth") || !strings.Contains(d.Reason, "sample rate") {
		t.Errorf("reason %q should name the better and the worse dimension", d.Reason)
	}
	if r.sha(inc.Local) != old {
		t.Fatal("the library changed while the question was open")
	}

	decs := NewDecisions(r.v.e, AnswerHooks{ApproveSwap: r.sw.ApproveSwap})
	if _, err := decs.Answer(context.Background(), d.ID, OptionApprove, ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if st := r.v.item(item).State; st != StateSwapping {
		t.Fatalf("item is %s after approval, want swapping", st)
	}
	r.drive(item, 8, StateDone)
	if r.audioOf(inc.Local) != "NEW-B B24 R44100" {
		t.Errorf("library holds %q, want the approved candidate", r.audioOf(inc.Local))
	}
}

// "keep" drops the item and blocks the candidate: a later visit does not ask about it again.
func TestSwapKeepAnswerBlocksTheCandidate(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "OLD-A B16 R96000", RG: "rg1"})
	item, _ := r.seedSwap(inc, swapCandOpts{Audio: "NEW-B B24 R44100"})
	r.mustStep(item)
	d := r.openDecision(item, DecisionTradeOff)
	decs := NewDecisions(r.v.e, AnswerHooks{ApproveSwap: r.sw.ApproveSwap})
	if _, err := decs.Answer(context.Background(), d.ID, OptionKeep, ""); err != nil {
		t.Fatal(err)
	}
	if st := r.v.item(item).State; st != StateDropped {
		t.Fatalf("item is %s, want dropped", st)
	}
	// The same candidate audio, on a fresh visit, is blocked without asking again.
	item2, _ := r.seedSwap(inc, swapCandOpts{Audio: "NEW-B B24 R44100"})
	r.mustStep(item2)
	if st := r.v.item(item2).State; st != StateDropped {
		t.Fatalf("second visit: item is %s (%q), want dropped because the candidate is blocked", st, r.v.item(item2).LastReason)
	}
	if r.v.openDecisions(item2) != 0 {
		t.Error("the blocked candidate raised another question")
	}
	// The sweep copies the answer into the blocked table.
	sw, _ := NewSweep(SweepConfig{DB: r.v.db, Now: r.v.clock.Now})
	if n, err := sw.settleKeeps(context.Background()); err != nil || n != 1 {
		t.Fatalf("settleKeeps = %d, %v, want 1", n, err)
	}
	if n := r.v.count(`SELECT COUNT(*) FROM mus_blocked_candidates WHERE recording_id = ? AND pcm_fingerprint = ?`, inc.RecID, placeFP("NEW-B B24 R44100")); n != 1 {
		t.Errorf("blocked rows = %d, want 1", n)
	}
}

// AE8: an explicit incumbent is never replaced by a clean candidate, even a higher-resolution one.
func TestSwapExplicitIncumbentNeverReplacedByClean(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "OLD-EXPLICIT", RG: "rg1", Explicit: true})
	item, _ := r.seedSwap(inc, swapCandOpts{Audio: "NEW-CLEAN B24 R96000", ReleaseName: "Artist - Gold (Clean) [FLAC 24-96]"})
	old := r.sha(inc.Local)
	r.mustStep(item)
	if st := r.v.item(item).State; st != StateDropped {
		t.Fatalf("item is %s (%q), want dropped", st, r.v.item(item).LastReason)
	}
	if !strings.Contains(r.v.item(item).LastReason, "explicit") {
		t.Errorf("reason %q should say explicit and clean never replace each other", r.v.item(item).LastReason)
	}
	if r.sha(inc.Local) != old || r.journalRows(JournalSwap) != 0 {
		t.Fatal("the explicit file was replaced")
	}
}

// A candidate from a different release group becomes a decision, not a swap.
func TestSwapDifferentReleaseGroupIsADecision(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "OLD-A", RG: "rg1"})
	item, _ := r.seedSwap(inc, swapCandOpts{Audio: "NEW-B B24", RG: "rg-other"})
	old := r.sha(inc.Local)
	r.mustStep(item)
	if st := r.v.item(item).State; st != StateWaitingBlair {
		t.Fatalf("item is %s, want waiting_blair", st)
	}
	r.openDecision(item, DecisionDifferentMaster)
	if r.sha(inc.Local) != old {
		t.Fatal("the library changed")
	}
}

// No release-group evidence on the candidate is a decision too.
func TestSwapNoReleaseGroupEvidenceIsADecision(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "OLD-A", RG: "rg1"})
	item, _ := r.seedSwap(inc, swapCandOpts{Audio: "NEW-B B24", NoRG: true})
	r.mustStep(item)
	if st := r.v.item(item).State; st != StateWaitingBlair {
		t.Fatalf("item is %s, want waiting_blair", st)
	}
	d := r.openDecision(item, DecisionDifferentMaster)
	if !strings.Contains(d.Reason, "doesn't say which album release it came from") {
		t.Errorf("reason %q should name the missing evidence", d.Reason)
	}
}

// 3 LU of loudness difference means a different mastering.
func TestSwapLoudnessThreeLUApartIsADecision(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "OLD-A L-9.0", RG: "rg1"})
	item, _ := r.seedSwap(inc, swapCandOpts{Audio: "NEW-B B24 L-12.0"})
	r.mustStep(item)
	d := r.openDecision(item, DecisionDifferentMaster)
	if !strings.Contains(d.Reason, "3.0 LU") {
		t.Errorf("reason %q should name the 3 LU difference", d.Reason)
	}
}

// Different PCM but the same 16 kHz spectral signature as a FAKE incumbent never wins (KTD19).
func TestSwapSameSpectralSignatureAsFakeIncumbentNeverWins(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "OLD-FAKE FAKE T16000", RG: "rg1"})
	item, _ := r.seedSwap(inc, swapCandOpts{Audio: "NEW-SAME-SHELF T16100"})
	old := r.sha(inc.Local)
	r.mustStep(item)
	it := r.v.item(item)
	if it.State != StateDropped || !strings.Contains(it.LastReason, "quality problem") {
		t.Fatalf("item is %s (%q), want dropped for the shared spectral signature", it.State, it.LastReason)
	}
	if r.sha(inc.Local) != old || r.journalRows(JournalSwap) != 0 {
		t.Fatal("the library changed")
	}
}

// AE15: a lossy .m4a with a passing FLAC is a decision, because the path would change; the
// question offers no "approve" the engine could not carry out.
func TestSwapLossyM4aToFlacIsAPathChangeDecision(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "OLD-YT LOSSY", Ext: ".m4a", RG: "rg1"})
	item, _ := r.seedSwap(inc, swapCandOpts{Audio: "NEW-FLAC B16"})
	old := r.sha(inc.Local)
	r.mustStep(item)
	if st := r.v.item(item).State; st != StateWaitingBlair {
		t.Fatalf("item is %s (%q), want waiting_blair", st, r.v.item(item).LastReason)
	}
	d := r.openDecision(item, DecisionPathChange)
	if decHasOption(d.Options, OptionApprove) {
		t.Errorf("options = %v: a path change cannot be approved", d.Options)
	}
	if r.sha(inc.Local) != old || r.journalRows(JournalSwap) != 0 || len(r.pr.libFlacs()) != 0 {
		t.Fatal("the library changed")
	}
	// The hook refuses a path-change approval outright, in case it is ever wired to one.
	if err := r.sw.ApproveSwap(context.Background(), d, r.v.item(item)); err == nil {
		t.Error("ApproveSwap accepted a path change")
	}
}

// KTD16: with automatic swaps off, a would-be swap becomes a decision and the library is
// unchanged; approving that decision swaps.
func TestSwapAutomaticSwapsOffBecomesADecision(t *testing.T) {
	r := newSwapHarness(t)
	r.auto = false
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "OLD-GOLD SUSPECT", RG: "rg1"})
	item, _ := r.seedSwap(inc, swapCandOpts{Audio: "NEW-GOLD"})
	old := r.sha(inc.Local)
	r.mustStep(item)
	if st := r.v.item(item).State; st != StateWaitingBlair {
		t.Fatalf("item is %s, want waiting_blair", st)
	}
	d := r.openDecision(item, DecisionSwapDisabled)
	if r.sha(inc.Local) != old || r.journalRows(JournalSwap) != 0 {
		t.Fatal("the library changed with automatic swaps off")
	}
	decs := NewDecisions(r.v.e, AnswerHooks{ApproveSwap: r.sw.ApproveSwap})
	if _, err := decs.Answer(context.Background(), d.ID, OptionApprove, ""); err != nil {
		t.Fatal(err)
	}
	r.drive(item, 8, StateDone)
	if r.audioOf(inc.Local) != "NEW-GOLD" {
		t.Error("the approved swap did not happen")
	}
}

// An approval does not override a rule that keeps the incumbent: approve a swap that was
// blocked meanwhile and the incumbent still stays.
func TestSwapApprovalDoesNotOverrideABlock(t *testing.T) {
	r := newSwapHarness(t)
	r.auto = false
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "OLD-GOLD SUSPECT", RG: "rg1"})
	item, cand := r.seedSwap(inc, swapCandOpts{Audio: "NEW-GOLD"})
	r.mustStep(item)
	d := r.openDecision(item, DecisionSwapDisabled)
	if err := sweepBlockCandidate(context.Background(), r.v.db, cand, inc.RecID, inc.TrackID, 0, r.v.clock.Now()); err != nil {
		t.Fatal(err)
	}
	decs := NewDecisions(r.v.e, AnswerHooks{ApproveSwap: r.sw.ApproveSwap})
	if _, err := decs.Answer(context.Background(), d.ID, OptionApprove, ""); err != nil {
		t.Fatal(err)
	}
	r.mustStep(item)
	if r.audioOf(inc.Local) != "OLD-GOLD SUSPECT" || r.journalRows(JournalSwap) != 0 {
		t.Fatal("a blocked candidate was swapped in after approval")
	}
}

// A swap whose destination no longer holds the recorded incumbent stops with that reason and
// changes nothing (KTD18).
func TestSwapDestinationChangedSinceRecordingStopsAndChangesNothing(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "OLD-GOLD SUSPECT", RG: "rg1"})
	item, _ := r.seedSwap(inc, swapCandOpts{Audio: "NEW-GOLD"})
	r.pl.crash = func(pt string) bool { return pt == "stage:temp_verified" }
	if err := r.step(item); !errors.Is(err, errCrash) {
		t.Fatalf("step err = %v, want the simulated crash", err)
	}
	r.pl.crash = nil
	// Someone replaces the library file between the journal recording the incumbent and the swap.
	placeWrite(t, inc.Local, "SOMEONE-ELSES-EDIT", TrackTags{Artist: "Band", Album: "Album", Title: "Gold", Track: 1, RecordingMBID: inc.MBID})
	edited := r.sha(inc.Local)

	r.mustStep(item)

	if r.sha(inc.Local) != edited {
		t.Fatal("the swap replaced a file it had not recorded")
	}
	if got := placeTemps(t, r.pr.lib); len(got) != 0 {
		t.Errorf("temp files left: %v", got)
	}
	if n := r.v.count(`SELECT COUNT(*) FROM mus_quarantine`); n != 0 {
		t.Errorf("quarantine rows = %d, want none", n)
	}
	it := r.v.item(item)
	if it.State != StateWaitingBlair {
		t.Fatalf("item is %s, want waiting_blair", it.State)
	}
	d := r.openDecision(item, "collision")
	if !strings.Contains(d.Reason, "is no longer the file that was there when the swap started") {
		t.Errorf("decision reason %q should say the destination no longer holds the incumbent", d.Reason)
	}
	var lastErr string
	var closed int64
	_ = r.v.db.QueryRow(`SELECT last_error, closed_at FROM mus_swap_journal`).Scan(&lastErr, &closed)
	if closed == 0 || !strings.HasPrefix(lastErr, "stopped:") {
		t.Errorf("journal closed_at %d last_error %q, want a closed, stopped row", closed, lastErr)
	}
}

// A candidate that fails the in-place check is rejected and the library is untouched.
func TestSwapCorruptStagedCandidateIsRejected(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "OLD-GOLD SUSPECT", RG: "rg1"})
	item, cand := r.seedSwap(inc, swapCandOpts{Audio: "NEW-GOLD"})
	old := r.sha(inc.Local)
	// The staged file is damaged after it was checked.
	var src string
	_ = r.v.db.QueryRow(`SELECT staging_path FROM mus_candidates WHERE id = ?`, cand).Scan(&src)
	placeWrite(t, src, "NEW-GOLD CORRUPT", TrackTags{Artist: "Band", Album: "Album", Title: "Gold", Track: 1, RecordingMBID: inc.MBID})
	r.exec(`UPDATE mus_candidates SET sha256 = '' WHERE id = ?`, cand)
	r.mustStep(item)
	if st := r.v.item(item).State; st != StateSearching {
		t.Fatalf("item is %s, want searching for the next candidate", st)
	}
	var cs string
	_ = r.v.db.QueryRow(`SELECT state FROM mus_candidates WHERE id = ?`, cand).Scan(&cs)
	if cs != "rejected" {
		t.Errorf("candidate state = %s, want rejected", cs)
	}
	if r.sha(inc.Local) != old || len(placeTemps(t, r.pr.lib)) != 0 {
		t.Fatal("library changed or a temp file was left")
	}
}

// U10 + U11 compose: a crash at each point of a real swap leaves exactly one complete copy;
// the boot reconciler settles it, a second reconcile changes nothing, and the swap finishes.
func TestSwapCrashAtEveryPointReconcilesToOneCompleteCopy(t *testing.T) {
	points := []struct {
		point   string
		rolled  bool // the reconciler rolls the row back; the item swaps again from the top
		swapped bool // the new file is already in the library at the crash
	}{
		{"stage:begin", true, false},
		{"stage:staged", true, false},
		{"stage:mid_copy", true, false},
		{"stage:copied", true, false},
		{"stage:tagged", true, false},
		{"stage:temp_verified", false, false},
		{"quarantine:mid_copy", false, false},
		{"quarantine:copied", false, false},
		{"quarantine:recorded", false, false},
		{"swap:renamed", false, true},
	}
	for _, tc := range points {
		t.Run(tc.point, func(t *testing.T) {
			r := newSwapHarness(t)
			inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "OLD-GOLD SUSPECT", RG: "rg1", Rating: 8, Playlists: []string{"Road trip"}})
			item, _ := r.seedSwap(inc, swapCandOpts{Audio: "NEW-GOLD"})
			oldSHA := r.sha(inc.Local)

			r.pl.crash = func(pt string) bool { return pt == tc.point }
			if err := r.step(item); !errors.Is(err, errCrash) {
				t.Fatalf("step err = %v, want the simulated crash at %s", err, tc.point)
			}
			if got := r.audioOf(inc.Local); tc.swapped != (got == "NEW-GOLD") {
				t.Fatalf("at the crash the library holds %q (swapped=%v expected)", got, tc.swapped)
			}

			r.build() // a new process
			rep, err := r.pl.Reconcile(context.Background())
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if tc.rolled && rep.RolledBack != 1 {
				t.Errorf("report %+v, want the row rolled back", rep)
			}
			if !tc.rolled && rep.Advanced != 1 {
				t.Errorf("report %+v, want the row advanced", rep)
			}
			before := r.pr.snapshot()
			if _, err := r.pl.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			if after := r.pr.snapshot(); after != before {
				t.Errorf("a second reconcile changed state:\nbefore %s\nafter  %s", before, after)
			}

			// Exactly one complete copy: the old or the new, never both, never a fragment.
			if flacs := r.pr.libFlacs(); len(flacs) != 1 || flacs[0] != inc.Local {
				t.Fatalf("library flacs = %v, want only %s", flacs, inc.Local)
			}
			if got := r.audioOf(inc.Local); got != "OLD-GOLD SUSPECT" && got != "NEW-GOLD" {
				t.Fatalf("library file holds %q", got)
			}
			if got := placeTemps(t, r.pr.lib); len(got) != 0 {
				t.Errorf("temp files left after reconcile: %v", got)
			}

			r.drive(item, 8, StateDone)
			if r.audioOf(inc.Local) != "NEW-GOLD" {
				t.Fatalf("library holds %q after the swap finished, want the candidate", r.audioOf(inc.Local))
			}
			// The incumbent is in quarantine exactly once per swap, whole.
			var qpath, qsha string
			if err := r.v.db.QueryRow(`SELECT path, sha256 FROM mus_quarantine`).Scan(&qpath, &qsha); err != nil {
				t.Fatalf("quarantine row: %v", err)
			}
			if qsha != oldSHA || r.sha(qpath) != oldSHA {
				t.Errorf("quarantined version sha %s file %s, want the incumbent %s", qsha, r.sha(qpath), oldSHA)
			}
			if got := r.ratingOf(r.keyOf(inc.TrackID)); got != 8 {
				t.Errorf("rating after the crashed swap = %v, want 8", got)
			}
			if !r.inPlaylist("Road trip", r.keyOf(inc.TrackID)) {
				t.Error("playlist membership lost across the crash")
			}
			if n := r.v.count(`SELECT COUNT(*) FROM mus_swap_journal WHERE closed_at = 0`); n != 0 {
				t.Errorf("open journal rows = %d", n)
			}
		})
	}
}

// The swap is driven by the engine's own Tick when the Placement is the engine's Placer.
func TestSwapDrivenByEngineTick(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "OLD-GOLD SUSPECT", RG: "rg1"})
	item, _ := r.seedSwap(inc, swapCandOpts{Audio: "NEW-GOLD"})
	e, err := New(Config{
		DB: r.v.db, Settings: func() Settings { return r.v.settings },
		Placer: r.pl, Now: r.v.clock.Now, Notifier: r.v.notifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.pl.Bind(e)
	for i := 0; i < 6 && r.v.item(item).State != StateDone; i++ {
		if err := e.Tick(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	if st := r.v.item(item).State; st != StateDone {
		t.Fatalf("item is %s (%q) after ticks", st, r.v.item(item).LastReason)
	}
	if r.audioOf(inc.Local) != "NEW-GOLD" {
		t.Error("the swap did not happen under Tick")
	}
}

func TestSwapsBindRefusesAQuarantineRootMismatch(t *testing.T) {
	r := newSwapHarness(t)
	other, err := NewQuarantine(QuarantineConfig{DB: r.v.db, Root: filepath.Join(t.TempDir(), "elsewhere")})
	if err != nil {
		t.Fatal(err)
	}
	sw, err := NewSwaps(SwapsConfig{Checker: r.chk, Plex: r.fp, Quarantine: other})
	if err != nil {
		t.Fatal(err)
	}
	if err := sw.Bind(r.pl); err == nil {
		t.Fatal("Bind accepted a quarantine root that differs from the placement's")
	}
}
