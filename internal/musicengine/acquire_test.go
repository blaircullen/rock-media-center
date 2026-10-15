package musicengine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tristenlammi/arrmada/internal/audiocheck"
	"github.com/tristenlammi/arrmada/internal/download"
	"github.com/tristenlammi/arrmada/internal/indexer"
	"github.com/tristenlammi/arrmada/internal/slskd"
	"github.com/tristenlammi/arrmada/internal/ytaudio"
)

// ---- fakes (prefixed acq so they never collide with other units' test helpers) ----

type acqFakeIdx struct {
	mu       sync.Mutex
	indexers []indexer.Indexer
	byCat    map[int][]indexer.Release
	failed   map[string]string // indexer name -> error: Search returns AllFailedError
	calls    []int             // categories searched, in order
	fetches  []string
}

func newAcqFakeIdx() *acqFakeIdx {
	return &acqFakeIdx{
		indexers: []indexer.Indexer{
			{Name: "NZBgeek", Kind: indexer.KindNewznab, Enabled: true},
			{Name: "TL", Kind: indexer.KindTorznab, Enabled: true},
		},
		byCat:  map[int][]indexer.Release{},
		failed: map[string]string{},
	}
}

func (f *acqFakeIdx) List(context.Context) ([]indexer.Indexer, error) { return f.indexers, nil }

func (f *acqFakeIdx) Search(_ context.Context, q indexer.SearchQuery) (indexer.SearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, q.Categories[0])
	if q.MediaType != indexer.MediaMusic {
		return indexer.SearchResult{}, errors.New("not a music search")
	}
	if len(f.failed) > 0 {
		return indexer.SearchResult{}, &indexer.AllFailedError{Errors: f.failed}
	}
	return indexer.SearchResult{Releases: f.byCat[q.Categories[0]], Asked: 2}, nil
}

func (f *acqFakeIdx) Fetch(_ context.Context, name, url string) (indexer.FetchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetches = append(f.fetches, name+" "+url)
	return indexer.FetchResult{File: []byte("nzb"), Filename: "x.nzb"}, nil
}

func (f *acqFakeIdx) searchCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type acqRelease struct {
	files []string
}

type acqFakeDL struct {
	mu      sync.Mutex
	rels    map[string]acqRelease // by release name
	sabDir  string
	queue   []download.Item
	adds    []download.AddRequest
	failed  map[string]string // release name -> FailMessage: job reports an error
	hold    map[string]bool   // release name -> never completes (still downloading)
	n       int
	removed []string
}

func newAcqFakeDL(t *testing.T) *acqFakeDL {
	return &acqFakeDL{rels: map[string]acqRelease{}, sabDir: t.TempDir(), failed: map[string]string{}, hold: map[string]bool{}}
}

func (f *acqFakeDL) AddDownload(_ context.Context, req download.AddRequest) (download.Added, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adds = append(f.adds, req)
	f.n++
	rel, ok := f.rels[req.Name]
	if !ok {
		return download.Added{}, fmt.Errorf("fake: unknown release %q", req.Name)
	}
	usenet := req.Transport == download.TransportUsenet
	base := req.SavePath
	key := fmt.Sprintf("%040x", f.n)
	if usenet {
		base = f.sabDir
		key = fmt.Sprintf("sab-nzo%d", f.n)
	}
	dir := filepath.Join(base, req.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return download.Added{}, err
	}
	for _, name := range rel.files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("audio"), 0o644); err != nil {
			return download.Added{}, err
		}
	}
	qi := download.Item{Hash: key, Name: req.Name, Category: req.Category, Progress: 1, Transport: download.Transport(req.Transport),
		State: "seeding", RawState: "uploading", ContentPath: dir}
	if usenet {
		qi.State = download.StateCompleted
		qi.RawState = "Completed"
	}
	switch {
	case f.failed[req.Name] != "":
		qi.State, qi.RawState, qi.FailMessage, qi.Progress = "error", "Failed", f.failed[req.Name], 0
	case f.hold[req.Name]:
		qi.State, qi.RawState, qi.Progress, qi.RemainingBytes = "downloading", "downloading", 0.4, 100
	}
	f.queue = append(f.queue, qi)
	return download.Added{ClientID: 1, Key: key}, nil
}

func (f *acqFakeDL) QueueComplete(context.Context) ([]download.Item, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]download.Item(nil), f.queue...), true, nil
}

func (f *acqFakeDL) Remove(_ context.Context, key string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, key)
	return nil
}

func (f *acqFakeDL) addCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.adds)
}

func (f *acqFakeDL) finish(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.queue {
		if f.queue[i].Name == name {
			f.queue[i].Progress, f.queue[i].RemainingBytes = 1, 0
			f.queue[i].State, f.queue[i].RawState = "seeding", "uploading"
			if f.queue[i].Transport == download.TransportUsenet {
				f.queue[i].State, f.queue[i].RawState = download.StateCompleted, "Completed"
			}
		}
	}
}

type acqFakeSlskd struct {
	mu       sync.Mutex
	files    []slskd.File
	err      error
	searches int
	enqueued []string
	phase    slskd.Phase
	dir      string // where the finished file appears
}

func (f *acqFakeSlskd) Search(_ context.Context, _ string, _ slskd.SearchOptions) ([]slskd.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searches++
	return f.files, f.err
}

func (f *acqFakeSlskd) Enqueue(_ context.Context, user, name string, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enqueued = append(f.enqueued, user+" "+name)
	return nil
}

func (f *acqFakeSlskd) GetDownload(_ context.Context, user, name string) (slskd.Transfer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slskd.Transfer{Username: user, Filename: name, Phase: f.phase, State: "Completed, Succeeded"}, nil
}

type acqFakeYT struct {
	mu      sync.Mutex
	hits    []ytaudio.Candidate
	err     error
	searchN int
	dlN     int
}

func (f *acqFakeYT) Search(context.Context, string, string) ([]ytaudio.Candidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searchN++
	return f.hits, f.err
}

func (f *acqFakeYT) Download(_ context.Context, url, dir string) (ytaudio.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dlN++
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ytaudio.Result{}, err
	}
	p := filepath.Join(dir, "abc123.m4a")
	if err := os.WriteFile(p, []byte("aac"), 0o644); err != nil {
		return ytaudio.Result{}, err
	}
	return ytaudio.Result{Path: p, SourceURL: url, Codec: "aac", BitrateBPS: 128000, Container: "m4a", Lossy: true}, nil
}

// acqChecker answers by file base name; each name's results are consumed in order and the
// last one repeats. An unknown name is BLIND.
type acqChecker struct {
	mu      sync.Mutex
	results map[string][]audiocheck.Result
	calls   map[string]int
}

func newAcqChecker() *acqChecker {
	return &acqChecker{results: map[string][]audiocheck.Result{}, calls: map[string]int{}}
}

func (c *acqChecker) set(base string, rs ...audiocheck.Result) { c.results[base] = rs }

func (c *acqChecker) Check(_ context.Context, path string) audiocheck.Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	base := filepath.Base(path)
	c.calls[base]++
	rs := c.results[base]
	if len(rs) == 0 {
		return audiocheck.Result{Path: path, Verdict: audiocheck.Blind, Reason: "no fake result for " + base}
	}
	r := rs[min(c.calls[base]-1, len(rs)-1)]
	r.Path = path
	return r
}

func acqGenuine() audiocheck.Result {
	r := genuineFLAC()
	r.Integrity = audiocheck.Integrity{OK: true, DecodedSec: 200}
	r.Fingerprint = "fp-genuine"
	r.Facts.Container = "flac"
	r.Facts.SizeBytes = 30 << 20
	return r
}

func acqVerdict(v audiocheck.Verdict) audiocheck.Result {
	r := acqGenuine()
	r.Verdict, r.Reason = v, "test "+string(v)
	if v == audiocheck.Fake {
		r.Integrity.OK = true
	}
	return r
}

type acqFakeSpace struct {
	mu       sync.Mutex
	ok       bool
	reason   string
	reserved []int64
	released int
}

func (s *acqFakeSpace) Reserve(_ context.Context, _ WorkKind, _ int64, bytes int64) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ok {
		return false, s.reason
	}
	s.reserved = append(s.reserved, bytes)
	return true, ""
}

func (s *acqFakeSpace) Release(context.Context, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released++
}

// ---- harness ----

type acqHarness struct {
	*env
	idx     *acqFakeIdx
	dl      *acqFakeDL
	slsk    *acqFakeSlskd
	yt      *acqFakeYT
	chk     *acqChecker
	space   *acqFakeSpace
	staging *Staging
	libRoot string
	vpnOK   bool
	acq     *Acquire
}

func newAcqHarness(t *testing.T) *acqHarness {
	t.Helper()
	h := &acqHarness{
		env: newEnv(t), idx: newAcqFakeIdx(), dl: newAcqFakeDL(t),
		slsk: &acqFakeSlskd{phase: slskd.PhaseSucceeded}, yt: &acqFakeYT{}, chk: newAcqChecker(),
		space: &acqFakeSpace{ok: true}, vpnOK: true, libRoot: t.TempDir(),
	}
	st, err := NewStaging(filepath.Join(t.TempDir(), "staging"), h.libRoot)
	if err != nil {
		t.Fatal(err)
	}
	h.staging = st
	h.slsk.dir = t.TempDir()
	h.acq = h.newAcquire()
	return h
}

// newAcquire builds a fresh Acquire over the same database, as a restart would.
func (h *acqHarness) newAcquire() *Acquire {
	h.t.Helper()
	a, err := NewAcquire(AcquireConfig{
		DB: h.db, Indexer: h.idx, Downloads: h.dl, Slskd: h.slsk, YouTube: h.yt,
		Checker: h.chk, Space: h.space, Staging: h.staging, SoulseekDir: h.slsk.dir,
		VPN: func(context.Context) (bool, string) {
			if h.vpnOK {
				return true, ""
			}
			return false, "gluetun is not attached"
		},
		Now: h.clock.Now,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return a
}

// newItem seeds a request item in searching for "Hang With Me" by Robyn (studio).
func (h *acqHarness) newItem(kind string, state State) int64 {
	h.t.Helper()
	rec := h.seedRecording("mbid-hwm", "Hang With Me", "Robyn", "SEA011000001", "studio")
	return h.seedItem(kind, state, rec)
}

func (h *acqHarness) step(id int64) error {
	h.t.Helper()
	return h.acq.Step(context.Background(), h.e, h.item(id))
}

// stepOK steps and fails the test on an error.
func (h *acqHarness) stepOK(id int64) {
	h.t.Helper()
	if err := h.step(id); err != nil {
		h.t.Fatalf("step: %v", err)
	}
}

func (h *acqHarness) usenetRelease(name string, size int64) indexer.Release {
	return indexer.Release{Title: name, DownloadURL: "https://nzb.example/get/" + name, Indexer: "NZBgeek", SizeBytes: size, Transport: indexer.TransportUsenet}
}

func (h *acqHarness) torrentRelease(name string, seeders int) indexer.Release {
	return indexer.Release{Title: name, DownloadURL: "magnet:?xt=" + name, Indexer: "TL", SizeBytes: 200 << 20, Seeders: seeders, Transport: indexer.TransportTorrent}
}

func (h *acqHarness) candidates(id int64) []struct{ Lane, State, Verdict, Name string } {
	h.t.Helper()
	rows, err := h.db.Query(`SELECT lane, state, verdict, release_name FROM mus_candidates WHERE item_id = ? ORDER BY id`, id)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []struct{ Lane, State, Verdict, Name string }
	for rows.Next() {
		var c struct{ Lane, State, Verdict, Name string }
		if err := rows.Scan(&c.Lane, &c.State, &c.Verdict, &c.Name); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

const (
	acqAlbum  = "Robyn - Body Talk Pt. 2 (2010) [FLAC]"
	acqAlbumB = "Robyn - Body Talk (2010) [FLAC]"
	acqTrack  = "02 - Hang With Me.flac"
)

func acqAlbumFiles() acqRelease {
	return acqRelease{files: []string{"01 - Include Me Out.flac", acqTrack, "03 - U Should Know Better.flac"}}
}

// ---- tests ----

// A usenet release that passes ends the search: torrents and Soulseek are never asked, the
// download goes to SABnzbd in the rmc-music category, the target track alone is checked, and
// the rest of the release stays in staging (R8, R10, R12).
func TestAcquireUsenetPassesAndOtherLanesAreNeverSearched(t *testing.T) {
	h := newAcqHarness(t)
	h.idx.byCat[3040] = []indexer.Release{h.usenetRelease(acqAlbum, 300<<20)}
	h.dl.rels[acqAlbum] = acqAlbumFiles()
	h.chk.set(acqTrack, acqGenuine())
	id := h.newItem(KindRequest, StateSearching)

	h.stepOK(id)

	it := h.item(id)
	if it.State != StatePlacing {
		t.Fatalf("state = %s (%s), want placing", it.State, it.LastReason)
	}
	if h.slsk.searches != 0 {
		t.Errorf("soulseek searched %d times, want 0", h.slsk.searches)
	}
	if got := h.idx.calls; len(got) != 1 || got[0] != 3040 {
		t.Errorf("indexer searches = %v, want only category 3040 once", got)
	}
	if len(h.dl.adds) != 1 || h.dl.adds[0].Category != "rmc-music" || h.dl.adds[0].Transport != download.TransportUsenet {
		t.Fatalf("adds = %+v", h.dl.adds)
	}
	if !strings.HasPrefix(h.dl.adds[0].SavePath, h.staging.Root()) {
		t.Errorf("save path %q is not under staging %q", h.dl.adds[0].SavePath, h.staging.Root())
	}
	if h.chk.calls[acqTrack] != 1 || len(h.chk.calls) != 1 {
		t.Errorf("checker calls = %v, want only the target track once", h.chk.calls)
	}
	var state, verdict, path string
	var integ int
	if err := h.db.QueryRow(`SELECT state, verdict, staging_path, integrity_ok FROM mus_candidates WHERE item_id = ?`, id).Scan(&state, &verdict, &path, &integ); err != nil {
		t.Fatal(err)
	}
	if state != "chosen" || verdict != "GENUINE" || integ != 1 || filepath.Base(path) != acqTrack {
		t.Errorf("candidate = %s %s %s integrity=%d", state, verdict, path, integ)
	}
	// The rest of the release is still in staging beside the target (R10).
	other := filepath.Join(filepath.Dir(path), "01 - Include Me Out.flac")
	if _, err := os.Stat(other); err != nil {
		t.Errorf("release remainder missing from staging: %v", err)
	}
	if h.count(`SELECT COUNT(*) FROM mus_candidates WHERE item_id = ? AND reserved_bytes <> 0`, id) != 0 {
		t.Error("reservation still recorded after the check")
	}
	if h.space.released == 0 || len(h.space.reserved) != 1 || h.space.reserved[0] != 600<<20 {
		t.Errorf("space: reserved %v released %d, want one reservation of twice the usenet size", h.space.reserved, h.space.released)
	}
}

// 3040 is searched first, then 3000 (KTD14).
func TestAcquireSearchesCategory3040Then3000(t *testing.T) {
	h := newAcqHarness(t)
	h.idx.byCat[3000] = []indexer.Release{h.usenetRelease(acqAlbum, 300<<20)}
	h.dl.rels[acqAlbum] = acqAlbumFiles()
	h.chk.set(acqTrack, acqGenuine())
	id := h.newItem(KindRequest, StateSearching)

	h.stepOK(id)

	if got := h.idx.calls; len(got) < 2 || got[0] != 3040 || got[1] != 3000 {
		t.Fatalf("categories searched = %v, want 3040 then 3000", got)
	}
	if st := h.item(id).State; st != StatePlacing {
		t.Fatalf("state = %s", st)
	}
}

// AE5: the first candidate is a transcode; it is rejected with its verdict, stays in staging,
// and the next candidate is tried.
func TestAcquireFakeFirstCandidateRejectedSecondTried(t *testing.T) {
	h := newAcqHarness(t)
	h.idx.byCat[3040] = []indexer.Release{h.usenetRelease(acqAlbum, 300<<20), h.usenetRelease(acqAlbumB, 250<<20)}
	h.dl.rels[acqAlbum] = acqAlbumFiles()
	h.dl.rels[acqAlbumB] = acqAlbumFiles()
	h.chk.set(acqTrack, acqVerdict(audiocheck.Fake), acqGenuine())
	id := h.newItem(KindRequest, StateSearching)

	h.stepOK(id)

	if st := h.item(id).State; st != StatePlacing {
		t.Fatalf("state = %s (%s)", st, h.item(id).LastReason)
	}
	cs := h.candidates(id)
	if len(cs) != 2 || cs[0].State != "rejected" || cs[0].Verdict != "FAKE" || cs[1].State != "chosen" {
		t.Fatalf("candidates = %+v", cs)
	}
	var dir string
	var expires int64
	var reason string
	if err := h.db.QueryRow(`SELECT id, expires_at, reject_reason FROM mus_candidates WHERE state = 'rejected'`).Scan(&dir, &expires, &reason); err != nil {
		t.Fatal(err)
	}
	if want := h.clock.Now().Add(StagingExpiry).Unix(); expires != want {
		t.Errorf("rejected candidate expires_at = %d, want %d", expires, want)
	}
	if !strings.Contains(reason, "FAKE") {
		t.Errorf("reject_reason = %q", reason)
	}
	if _, err := os.Stat(filepath.Join(h.staging.Root(), fmt.Sprint(id), dir)); err != nil {
		t.Errorf("rejected candidate's files left staging early: %v", err)
	}
}

// Every candidate failing sends the item to waiting_blair with the three choices (R15).
func TestAcquireAllCandidatesFailRaisesTheThreeChoices(t *testing.T) {
	h := newAcqHarness(t)
	h.idx.byCat[3040] = []indexer.Release{h.usenetRelease(acqAlbum, 300<<20)}
	h.dl.rels[acqAlbum] = acqAlbumFiles()
	h.chk.set(acqTrack, acqVerdict(audiocheck.Fake))
	id := h.newItem(KindRequest, StateSearching)

	h.stepOK(id)

	it := h.item(id)
	if it.State != StateWaitingBlair {
		t.Fatalf("state = %s (%s), want waiting_blair", it.State, it.LastReason)
	}
	var kind, opts string
	if err := h.db.QueryRow(`SELECT kind, options FROM mus_decisions WHERE item_id = ? AND answered_at = 0`, id).Scan(&kind, &opts); err != nil {
		t.Fatal(err)
	}
	if kind != DecisionNoGenuineCopy || opts != `["lossy","wait","drop"]` {
		t.Errorf("decision = %s %s", kind, opts)
	}
	if !strings.Contains(it.LastReason, "failed the checks") {
		t.Errorf("reason = %q", it.LastReason)
	}
	// A further step changes nothing (waiting_blair is not an acquirer state) and raises no second decision.
	h.stepOK(id)
	if n := h.openDecisions(id); n != 1 {
		t.Errorf("open decisions = %d", n)
	}
}

// All lanes searched and nothing found at all is the same decision, with the lanes named.
func TestAcquireAllLanesMissRaisesDecision(t *testing.T) {
	h := newAcqHarness(t)
	id := h.newItem(KindRequest, StateSearching)

	h.stepOK(id)

	it := h.item(id)
	if it.State != StateWaitingBlair || h.openDecisions(id) != 1 {
		t.Fatalf("state = %s decisions = %d", it.State, h.openDecisions(id))
	}
	for _, lane := range []string{LaneUsenet, LaneTorrent, LaneSoulseek} {
		if !strings.Contains(it.LastReason, lane) {
			t.Errorf("reason %q does not name %s", it.LastReason, lane)
		}
	}
	if h.count(`SELECT COUNT(*) FROM mus_lane_status WHERE status = 'available'`) != 3 {
		t.Error("all three lanes should be recorded as available (searched)")
	}
}

// A candidate whose reservation would cross the floor is not started (R27).
func TestAcquireReservationRefusedDoesNotStart(t *testing.T) {
	h := newAcqHarness(t)
	h.idx.byCat[3040] = []indexer.Release{h.usenetRelease(acqAlbum, 300<<20)}
	h.dl.rels[acqAlbum] = acqAlbumFiles()
	h.chk.set(acqTrack, acqGenuine())
	h.space.ok, h.space.reason = false, "downloads volume would drop below the 100 GB floor"
	id := h.newItem(KindRequest, StateSearching)

	h.stepOK(id)

	it := h.item(id)
	if it.State != StateSearching || !strings.Contains(it.LastReason, "waiting for space") {
		t.Fatalf("state = %s reason = %q", it.State, it.LastReason)
	}
	if h.dl.addCount() != 0 {
		t.Error("a download was started without a reservation")
	}
	if cs := h.candidates(id); len(cs) != 1 || cs[0].State != "found" {
		t.Fatalf("candidates = %+v, want one untried", cs)
	}
	if it.NextAttemptAt <= h.clock.Now().Unix() {
		t.Error("the item should wait before asking again")
	}
	// Space frees up: the same candidate starts, with no second search for it.
	h.space.ok = true
	h.stepOK(id)
	if st := h.item(id).State; st != StatePlacing {
		t.Fatalf("after space freed: %s (%s)", st, h.item(id).LastReason)
	}
	if n := len(h.candidates(id)); n != 1 {
		t.Errorf("candidates = %d, want the same one", n)
	}
}

// A BLIND check is retried and never counted as bad audio (R14); a candidate that stays
// unreadable is dropped without a bad-audio verdict.
func TestAcquireBlindCheckIsRetriedNotRejected(t *testing.T) {
	h := newAcqHarness(t)
	h.idx.byCat[3040] = []indexer.Release{h.usenetRelease(acqAlbum, 300<<20)}
	h.dl.rels[acqAlbum] = acqAlbumFiles()
	h.chk.set(acqTrack, verdictResult(audiocheck.Blind, "flac"), acqGenuine())
	id := h.newItem(KindRequest, StateSearching)

	if err := h.step(id); err == nil || !strings.Contains(err.Error(), "blind") {
		t.Fatalf("first step error = %v, want a blind-check error for the engine to back off on", err)
	}
	if it := h.item(id); it.State != StateChecking {
		t.Fatalf("state = %s, want checking", it.State)
	}
	if cs := h.candidates(id); cs[0].State != "downloading" {
		t.Fatalf("candidate = %+v, a blind check must not reject it", cs)
	}
	h.stepOK(id)
	if st := h.item(id).State; st != StatePlacing {
		t.Fatalf("after the retry: %s", st)
	}

	// Stays blind: third look rejects with verdict BLIND, never FAKE.
	h2 := newAcqHarness(t)
	h2.idx.byCat[3040] = []indexer.Release{h2.usenetRelease(acqAlbum, 300<<20)}
	h2.dl.rels[acqAlbum] = acqAlbumFiles()
	h2.chk.set(acqTrack, verdictResult(audiocheck.Blind, "flac"))
	id2 := h2.newItem(KindRequest, StateSearching)
	_ = h2.step(id2)
	if _, err := h2.db.Exec(`UPDATE mus_items SET attempts = ? WHERE id = ?`, acqBlindTries-1, id2); err != nil {
		t.Fatal(err)
	}
	h2.stepOK(id2)
	cs := h2.candidates(id2)
	if len(cs) == 0 || cs[0].State != "rejected" || cs[0].Verdict != "BLIND" {
		t.Fatalf("candidates = %+v", cs)
	}
}

// An upgrade item goes to swapping, a request to placing; the chosen file is the target
// track (staging_path), ready for U10/U11.
func TestAcquireUpgradeItemGoesToSwapping(t *testing.T) {
	h := newAcqHarness(t)
	h.idx.byCat[3040] = []indexer.Release{h.usenetRelease(acqAlbum, 300<<20)}
	h.dl.rels[acqAlbum] = acqAlbumFiles()
	h.chk.set(acqTrack, acqGenuine())
	id := h.newItem(KindUpgrade, StateSearching)

	h.stepOK(id)

	if st := h.item(id).State; st != StateSwapping {
		t.Fatalf("state = %s", st)
	}
}

// A release without the requested song, or holding only another version of it, fails
// without running the checker (R6).
func TestAcquireTargetTrackMissingOrWrongVersionIsRejected(t *testing.T) {
	h := newAcqHarness(t)
	h.idx.byCat[3040] = []indexer.Release{h.usenetRelease(acqAlbum, 300<<20)}
	h.dl.rels[acqAlbum] = acqRelease{files: []string{"01 - Include Me Out.flac", "02 - Hang With Me (Live).flac"}}
	id := h.newItem(KindRequest, StateSearching)

	h.stepOK(id)

	if st := h.item(id).State; st != StateWaitingBlair {
		t.Fatalf("state = %s (%s)", st, h.item(id).LastReason)
	}
	cs := h.candidates(id)
	if len(cs) != 1 || cs[0].State != "rejected" {
		t.Fatalf("candidates = %+v", cs)
	}
	if len(h.chk.calls) != 0 {
		t.Errorf("checker ran on %v; no file was the studio recording", h.chk.calls)
	}
	var reason string
	_ = h.db.QueryRow(`SELECT reject_reason FROM mus_candidates WHERE item_id = ?`, id).Scan(&reason)
	if !strings.Contains(reason, "not found") {
		t.Errorf("reject_reason = %q", reason)
	}
}

// A job the client reports as failed rejects the candidate and the next one is tried.
func TestAcquireClientFailureTriesNextCandidate(t *testing.T) {
	h := newAcqHarness(t)
	h.idx.byCat[3040] = []indexer.Release{h.usenetRelease(acqAlbum, 300<<20), h.usenetRelease(acqAlbumB, 250<<20)}
	h.dl.rels[acqAlbum] = acqAlbumFiles()
	h.dl.rels[acqAlbumB] = acqAlbumFiles()
	h.dl.failed[acqAlbum] = "out of retention"
	h.chk.set(acqTrack, acqGenuine())
	id := h.newItem(KindRequest, StateSearching)

	h.stepOK(id)

	if st := h.item(id).State; st != StatePlacing {
		t.Fatalf("state = %s (%s)", st, h.item(id).LastReason)
	}
	var reason string
	if err := h.db.QueryRow(`SELECT reject_reason FROM mus_candidates WHERE state = 'rejected'`).Scan(&reason); err != nil || !strings.Contains(reason, "out of retention") {
		t.Fatalf("reject_reason = %q, %v", reason, err)
	}
}

// After a restart the item is handed over again in downloading: the job is not added twice.
func TestAcquireResumesDownloadAfterRestartWithoutReAdding(t *testing.T) {
	h := newAcqHarness(t)
	h.idx.byCat[3040] = []indexer.Release{h.usenetRelease(acqAlbum, 300<<20)}
	h.dl.rels[acqAlbum] = acqAlbumFiles()
	h.dl.hold[acqAlbum] = true
	h.chk.set(acqTrack, acqGenuine())
	id := h.newItem(KindRequest, StateSearching)

	h.stepOK(id)
	if st := h.item(id).State; st != StateDownloading {
		t.Fatalf("state = %s", st)
	}
	h.acq = h.newAcquire() // restart
	h.stepOK(id)
	if st := h.item(id).State; st != StateDownloading || h.dl.addCount() != 1 {
		t.Fatalf("still downloading expected: state %s adds %d", st, h.dl.addCount())
	}
	h.dl.finish(acqAlbum)
	h.acq = h.newAcquire()
	h.stepOK(id)
	if st := h.item(id).State; st != StatePlacing || h.dl.addCount() != 1 {
		t.Fatalf("state = %s adds = %d, want placing after one add", st, h.dl.addCount())
	}
}

// R15 lossy path: Blair's "lossy" answer queues the YouTube audio; the file is checked for
// integrity only, labelled lossy, and keeps its container.
func TestAcquireAcceptLossyUsesYouTubeAudio(t *testing.T) {
	h := newAcqHarness(t)
	id := h.newItem(KindRequest, StateSearching)
	h.stepOK(id) // nothing anywhere
	if st := h.item(id).State; st != StateWaitingBlair {
		t.Fatalf("state = %s", st)
	}
	h.yt.hits = []ytaudio.Candidate{{URL: "https://youtu.be/abc123", DurationSec: 200, Topic: true}}
	lossy := acqGenuine()
	lossy.Verdict = audiocheck.Suspect // a lossy file is never "genuine lossless"; only integrity matters here
	lossy.Facts.Codec, lossy.Facts.Container = "aac", "m4a"
	h.chk.set("abc123.m4a", lossy)

	if err := h.acq.AcceptLossy(context.Background(), h.e, id); err != nil {
		t.Fatal(err)
	}
	if st := h.item(id).State; st != StateSearching {
		t.Fatalf("state after AcceptLossy = %s", st)
	}
	h.stepOK(id)

	if st := h.item(id).State; st != StatePlacing {
		t.Fatalf("state = %s (%s)", st, h.item(id).LastReason)
	}
	var lane, verdict, container, path string
	if err := h.db.QueryRow(`SELECT lane, verdict, container, staging_path FROM mus_candidates WHERE state = 'chosen'`).Scan(&lane, &verdict, &container, &path); err != nil {
		t.Fatal(err)
	}
	if lane != LaneYouTube || verdict != "LOSSY" || container != "m4a" || filepath.Ext(path) != ".m4a" {
		t.Errorf("chosen = %s %s %s %s", lane, verdict, container, path)
	}
	// A second approval is refused.
	if _, err := h.db.Exec(`UPDATE mus_items SET state = 'waiting_blair' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if err := h.acq.AcceptLossy(context.Background(), h.e, id); err == nil {
		t.Error("AcceptLossy twice should fail")
	}
}

func TestAcquireAcceptLossyOnlyFromWaitingBlair(t *testing.T) {
	h := newAcqHarness(t)
	id := h.newItem(KindRequest, StateSearching)
	if err := h.acq.AcceptLossy(context.Background(), h.e, id); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
}

// When the YouTube audio fails too, the decision is raised again without the lossy option.
func TestAcquireYouTubeFailureRaisesDecisionWithoutLossyOption(t *testing.T) {
	h := newAcqHarness(t)
	id := h.newItem(KindRequest, StateSearching)
	h.stepOK(id)
	// the first decision is answered by U12/U13; simulate that.
	if _, err := h.db.Exec(`UPDATE mus_decisions SET answered_at = 1, answer = 'lossy' WHERE item_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	h.yt.hits = nil // nothing of the right length
	if err := h.acq.AcceptLossy(context.Background(), h.e, id); err != nil {
		t.Fatal(err)
	}
	h.stepOK(id)
	if st := h.item(id).State; st != StateWaitingBlair {
		t.Fatalf("state = %s (%s)", st, h.item(id).LastReason)
	}
	var opts string
	if err := h.db.QueryRow(`SELECT options FROM mus_decisions WHERE item_id = ? AND answered_at = 0`, id).Scan(&opts); err != nil {
		t.Fatal(err)
	}
	if opts != `["wait","drop"]` {
		t.Errorf("options = %s", opts)
	}
}

// An item with no pinned recording cannot be searched; the error is clear, not a panic.
func TestAcquireItemWithoutRecordingErrors(t *testing.T) {
	h := newAcqHarness(t)
	id := h.seedItem(KindRequest, StateSearching, 0)
	if err := h.step(id); err == nil || !strings.Contains(err.Error(), "no pinned recording") {
		t.Fatalf("err = %v", err)
	}
}

func TestNewAcquireRequiresPieces(t *testing.T) {
	h := newAcqHarness(t)
	if _, err := NewAcquire(AcquireConfig{DB: h.db, Checker: h.chk}); err == nil {
		t.Error("missing staging should fail")
	}
	if _, err := NewAcquire(AcquireConfig{DB: h.db, Staging: h.staging}); err == nil {
		t.Error("missing checker should fail")
	}
	if _, err := NewAcquire(AcquireConfig{Staging: h.staging, Checker: h.chk}); err == nil {
		t.Error("missing database should fail")
	}
}
