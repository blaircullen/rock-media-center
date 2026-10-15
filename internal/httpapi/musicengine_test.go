package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tristenlammi/arrmada/internal/audiocheck"
	"github.com/tristenlammi/arrmada/internal/auth"
	"github.com/tristenlammi/arrmada/internal/musicengine"
)

// ---- harness ----

const meBase = "/api/v1/musicengine"

// newMEServer is a route server whose handler also carries the engine routes. server.go
// does not call registerMusicEngineRoutes until U15 wires it, so this builds the same
// middleware chain New does, around a router holding just those routes.
func newMEServer(t *testing.T, m *MusicEngineAPI) *routeServer {
	t.Helper()
	s := newRouteServer(t, nil)
	if m.DB == nil {
		m.DB = s.st.DB()
	}
	s.mountME(m)
	return s
}

// mountME (re)builds the handler around a fresh api over the same deps, as a restart of
// the HTTP layer would.
func (s *routeServer) mountME(m *MusicEngineAPI) {
	a := &api{deps: s.deps, start: time.Now(), loginLimiter: newLoginLimiter(10, 15*time.Minute),
		icsLimiter: newLoginLimiter(icsPerMinute, time.Minute), plexPins: newPlexPinGuard(), jfQC: newJFQCGuard()}
	rt := newRouter(a)
	a.registerMusicEngineRoutes(rt, m)
	s.h = a.recoverPanics(a.securityHeaders(a.authenticate(a.externalGate(a.logRequests(a.compressJSON(rt))))))
}

func meJSON(t *testing.T, rec *httptest.ResponseRecorder, want int) map[string]any {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("HTTP %d, want %d: %s", rec.Code, want, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON (%v): %s", err, rec.Body)
	}
	return out
}

func meExec(t *testing.T, db *sql.DB, q string, args ...any) int64 {
	t.Helper()
	res, err := db.Exec(q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	id, _ := res.LastInsertId()
	return id
}

// meItem inserts an item in the given state; its song is a recording row.
func meSeedItem(t *testing.T, db *sql.DB, kind, state, artist, title string) int64 {
	t.Helper()
	rec := meExec(t, db, `INSERT INTO mus_recordings (mbid, title, artist, created_at) VALUES (?, ?, ?, 1)`,
		fmt.Sprintf("mbid-%s-%s", artist, title), title, artist)
	dl := int64(0)
	if state == "searching" {
		dl = 5000
	}
	return meExec(t, db, `INSERT INTO mus_items (kind, recording_id, state, entered_at, deadline_at, last_reason, priority, created_at, updated_at)
		VALUES (?, ?, ?, 1000, ?, 'because', 100, 1000, 1000)`, kind, rec, state, dl)
}

func meSeedDecision(t *testing.T, db *sql.DB, item int64, kind string, opts ...string) int64 {
	t.Helper()
	raw, _ := json.Marshal(opts)
	return meExec(t, db, `INSERT INTO mus_decisions (item_id, kind, reason, options, created_at) VALUES (?, ?, 'why', ?, 2000)`,
		item, kind, string(raw))
}

func meItemState(t *testing.T, db *sql.DB, id int64) string {
	t.Helper()
	var st string
	if err := db.QueryRow(`SELECT state FROM mus_items WHERE id = ?`, id).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

// ---- fakes ----

type meFakeSwaps struct {
	err error
	res musicengine.UndoResult
	ids []int64
}

func (f *meFakeSwaps) Undo(_ context.Context, id int64) (musicengine.UndoResult, error) {
	f.ids = append(f.ids, id)
	return f.res, f.err
}

type meFakeQuarantine struct {
	clearedAge time.Duration
	report     musicengine.ClearReport
	err        error
	usage      musicengine.QuarantineUsage
}

func (f *meFakeQuarantine) Clear(_ context.Context, age time.Duration) (musicengine.ClearReport, error) {
	f.clearedAge = age
	return f.report, f.err
}
func (f *meFakeQuarantine) Usage(context.Context) (musicengine.QuarantineUsage, error) {
	return f.usage, nil
}

type meFakeSpace struct{ st musicengine.SpaceStatus }

func (f meFakeSpace) Status(context.Context) (musicengine.SpaceStatus, error) { return f.st, nil }

type meFakeAlbum struct {
	mu      sync.Mutex
	plan    musicengine.AlbumPlan
	planErr error
	placed  chan musicengine.AlbumOptions
	confirm chan int64
}

func (f *meFakeAlbum) Plan(context.Context, int64) (musicengine.AlbumPlan, error) {
	return f.plan, f.planErr
}
func (f *meFakeAlbum) Place(_ context.Context, _ int64, o musicengine.AlbumOptions) (musicengine.AlbumResult, error) {
	f.placed <- o
	return musicengine.AlbumResult{}, nil
}
func (f *meFakeAlbum) ConfirmAlbum(_ context.Context, id int64, _ string) (musicengine.AlbumResult, error) {
	f.confirm <- id
	return musicengine.AlbumResult{}, nil
}

type meFakeRemapper struct{ ran chan struct{} }

func (f *meFakeRemapper) Remap(context.Context) (musicengine.RemapReport, error) {
	f.ran <- struct{}{}
	return musicengine.RemapReport{}, nil
}

type meFakeBeat struct{ at int64 }

func (f meFakeBeat) Status() musicengine.HeartbeatStatus {
	return musicengine.HeartbeatStatus{LastTickAt: f.at}
}

type meFakeChecker struct {
	verdicts map[string]audiocheck.Verdict
	calls    []string
}

func (f *meFakeChecker) Check(_ context.Context, path string) audiocheck.Result {
	f.calls = append(f.calls, path)
	v, ok := f.verdicts[path]
	if !ok {
		v = audiocheck.Blind
	}
	return audiocheck.Result{Path: path, Verdict: v, Reason: "fake"}
}

// meRealDecisions builds the real decision list over the test database.
func meRealDecisions(t *testing.T, db *sql.DB) *musicengine.Decisions {
	t.Helper()
	e, err := musicengine.New(musicengine.Config{DB: db})
	if err != nil {
		t.Fatal(err)
	}
	return musicengine.NewDecisions(e, musicengine.AnswerHooks{})
}

// ---- access ----

// Every engine route is staff-only except the heartbeat: requesters and read-only accounts
// get 403, a visitor with no session gets 401, and a manager is let through.
func TestMusicEngineRoutesAreStaffOnlyExceptHeartbeat(t *testing.T) {
	m := &MusicEngineAPI{Heartbeat: meFakeBeat{at: 77}}
	s := newMEServer(t, m)
	_, requester := s.user(t, "kid", auth.RoleRequester)
	_, readonly := s.user(t, "ro", auth.RoleReadonly)
	_, manager := s.user(t, "mgr", auth.RoleManager)

	routes := []struct{ method, path, body string }{
		{"GET", "/items", ""}, {"GET", "/items/1", ""}, {"POST", "/items/1/full-album", `{"confirm":false}`},
		{"GET", "/decisions?open=1", ""}, {"POST", "/decisions/1/answer", `{"option":"drop"}`},
		{"GET", "/swaps", ""}, {"POST", "/swaps/1/undo", ""},
		{"GET", "/lanes", ""}, {"GET", "/space", ""},
		{"GET", "/quarantine/preview?older_than_days=90", ""}, {"POST", "/quarantine/clear", `{"older_than_days":90}`},
		{"GET", "/alerts/failures", ""},
		{"GET", "/settings", ""}, {"PUT", "/settings", `{"enabled":false}`},
		{"GET", "/release-checks", ""},
		{"POST", "/release-checks/calibration", `{"rows":[]}`}, {"POST", "/release-checks/path-test", `{"passed":false,"summary":""}`},
	}
	for _, rt := range routes {
		for name, c := range map[string]*http.Cookie{"requester": requester, "readonly": readonly} {
			if rec := s.doJSON(rt.method, meBase+rt.path, c, rt.body); rec.Code != http.StatusForbidden {
				t.Errorf("%s %s as %s: HTTP %d, want 403", rt.method, rt.path, name, rec.Code)
			}
		}
		if rec := s.doJSON(rt.method, meBase+rt.path, nil, rt.body); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with no session: HTTP %d, want 401", rt.method, rt.path, rec.Code)
		}
		if rec := s.doJSON(rt.method, meBase+rt.path, manager, rt.body); rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
			t.Errorf("%s %s as manager: HTTP %d, want it let through", rt.method, rt.path, rec.Code)
		}
	}

	// The heartbeat needs no session and is any account's to read.
	for name, c := range map[string]*http.Cookie{"anonymous": nil, "requester": requester} {
		if rec := s.do("GET", meBase+"/heartbeat", c); rec.Code != http.StatusOK {
			t.Errorf("heartbeat as %s: HTTP %d, want 200", name, rec.Code)
		}
	}
}

// The heartbeat says the last tick and nothing else, and is closed to the internet: the
// watchdog calls it on the LAN.
func TestMusicEngineHeartbeatIsOnlyLastTickAndLANOnly(t *testing.T) {
	s := newMEServer(t, &MusicEngineAPI{Heartbeat: meFakeBeat{at: 1760000000}})
	body := meJSON(t, s.do("GET", meBase+"/heartbeat", nil), http.StatusOK)
	if len(body) != 1 || body["last_tick"] != float64(1760000000) {
		t.Errorf("heartbeat body = %v, want exactly {last_tick: 1760000000}", body)
	}

	r := httptest.NewRequest("GET", "http://arrmada.local"+meBase+"/heartbeat", nil)
	r.RemoteAddr = "203.0.113.50:5000" // an internet visitor
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("heartbeat from outside the LAN: HTTP %d, want 403", rec.Code)
	}

	// Before the first tick: zero, still the only field.
	s2 := newMEServer(t, &MusicEngineAPI{Heartbeat: meFakeBeat{}})
	if body := meJSON(t, s2.do("GET", meBase+"/heartbeat", nil), http.StatusOK); len(body) != 1 || body["last_tick"] != float64(0) {
		t.Errorf("before the first tick: %v", body)
	}
}

// With the engine not built, every route answers 503 rather than vanishing, so the route
// table is the same either way.
func TestMusicEngineRoutesAnswer503WithoutAnEngine(t *testing.T) {
	s := newRouteServer(t, nil)
	s.mountME(nil)
	_, admin := s.user(t, "admin", auth.RoleAdmin)
	for _, p := range []string{"/items", "/settings", "/heartbeat"} {
		if rec := s.do("GET", meBase+p, admin); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s: HTTP %d, want 503", p, rec.Code)
		}
	}
}

// ---- items ----

func TestMusicEngineItemsListFilterPageAndDetail(t *testing.T) {
	s := newMEServer(t, &MusicEngineAPI{})
	db := s.st.DB()
	_, admin := s.user(t, "admin", auth.RoleAdmin)
	a := meSeedItem(t, db, "request", "searching", "Artist A", "Song A")
	b := meSeedItem(t, db, "sweep", "done", "Artist B", "Song B")
	c := meSeedItem(t, db, "request", "searching", "Artist C", "Song C")
	meExec(t, db, `INSERT INTO mus_candidates (item_id, lane, state, release_name, verdict, reject_reason, codec, bit_depth, sample_rate, size_bytes, created_at, checked_at)
		VALUES (?, 'usenet', 'rejected', 'Song.A.FLAC', 'FAKE', 'lossy transcode', 'flac', 16, 44100, 123, 1, 0)`, a)
	meExec(t, db, `INSERT INTO mus_swap_journal (kind, item_id, phase, started_at, phase_at, closed_at, last_error) VALUES ('place', ?, 5, 10, 10, 11, '')`, a)
	d := meSeedDecision(t, db, a, "no_genuine_copy", "lossy", "wait", "drop")

	list := meJSON(t, s.do("GET", meBase+"/items?state=searching", admin), 200)
	items := list["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["id"] != float64(c) || items[1].(map[string]any)["id"] != float64(a) {
		t.Fatalf("searching items = %v, want newest first [%d %d]", items, c, a)
	}
	first := items[0].(map[string]any)
	if first["title"] != "Song C" || first["artist"] != "Artist C" || first["kind"] != "request" || first["last_reason"] != "because" || first["deadline_at"] != float64(5000) {
		t.Errorf("item fields = %v", first)
	}
	if list["next_before_id"] != nil {
		t.Errorf("next_before_id = %v, want null at the end", list["next_before_id"])
	}

	all := meJSON(t, s.do("GET", meBase+"/items?limit=2", admin), 200)
	if n := len(all["items"].([]any)); n != 2 || all["next_before_id"] != float64(b) {
		t.Fatalf("page 1: %d items, next_before_id %v, want 2 and %d", n, all["next_before_id"], b)
	}
	page2 := meJSON(t, s.do("GET", fmt.Sprintf("%s/items?limit=2&before_id=%d", meBase, b), admin), 200)
	if ids := page2["items"].([]any); len(ids) != 1 || ids[0].(map[string]any)["id"] != float64(a) {
		t.Errorf("page 2 = %v, want just item %d", ids, a)
	}
	sweep := meJSON(t, s.do("GET", meBase+"/items?state=done", admin), 200)["items"].([]any)
	if len(sweep) != 1 || sweep[0].(map[string]any)["kind"] != "sweep" || sweep[0].(map[string]any)["deadline_at"] != nil {
		t.Errorf("done item = %v, want kind sweep and no deadline", sweep)
	}
	if rec := s.do("GET", meBase+"/items?state=Bad%20State", admin); rec.Code != http.StatusBadRequest {
		t.Errorf("bad state: HTTP %d, want 400", rec.Code)
	}

	det := meJSON(t, s.do("GET", fmt.Sprintf("%s/items/%d", meBase, a), admin), 200)
	if det["title"] != "Song A" || len(det["candidates"].([]any)) != 1 || len(det["journal"].([]any)) != 1 {
		t.Fatalf("detail = %v", det)
	}
	cand := det["candidates"].([]any)[0].(map[string]any)
	if cand["lane"] != "usenet" || cand["reject_reason"] != "lossy transcode" || cand["checked_at"] != nil || cand["bit_depth"] != float64(16) {
		t.Errorf("candidate = %v", cand)
	}
	decs := det["decisions"].([]any)
	if len(decs) != 1 || decs[0].(map[string]any)["id"] != float64(d) || decs[0].(map[string]any)["item"].(map[string]any)["title"] != "Song A" {
		t.Errorf("decisions = %v", decs)
	}
	if rec := s.do("GET", meBase+"/items/9999", admin); rec.Code != http.StatusNotFound {
		t.Errorf("unknown item: HTTP %d, want 404", rec.Code)
	}
}

// ---- decisions ----

// Answering moves the item: drop sends a waiting item to dropped, retry back to its step,
// through the real decision list.
func TestMusicEngineAnswerMovesTheItem(t *testing.T) {
	s := newMEServer(t, &MusicEngineAPI{})
	db := s.st.DB()
	s.mountME(&MusicEngineAPI{DB: db, Decisions: meRealDecisions(t, db)})
	_, admin := s.user(t, "admin", auth.RoleAdmin)

	item := meSeedItem(t, db, "request", "waiting_blair", "Artist", "Song")
	dec := meSeedDecision(t, db, item, "no_genuine_copy", "lossy", "wait", "drop")

	open := meJSON(t, s.do("GET", meBase+"/decisions?open=1", admin), 200)["decisions"].([]any)
	if len(open) != 1 || open[0].(map[string]any)["kind"] != "no_genuine_copy" || open[0].(map[string]any)["answer"] != nil ||
		open[0].(map[string]any)["item"].(map[string]any)["state"] != "waiting_blair" {
		t.Fatalf("open decisions = %v", open)
	}

	path := fmt.Sprintf("%s/decisions/%d/answer", meBase, dec)
	if rec := s.doJSON("POST", path, admin, `{"option":"explode"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad option: HTTP %d, want 400", rec.Code)
	}
	if got := meItemState(t, db, item); got != "waiting_blair" {
		t.Fatalf("a refused answer moved the item to %s", got)
	}
	res := meJSON(t, s.doJSON("POST", path, admin, `{"option":"drop","note":"not worth it"}`), 200)
	if res["ignored"] != false || res["state"] != "dropped" {
		t.Errorf("answer = %v, want not ignored, state dropped", res)
	}
	if got := meItemState(t, db, item); got != "dropped" {
		t.Errorf("item is %s, want dropped", got)
	}
	if rec := s.doJSON("POST", path, admin, `{"option":"drop"}`); rec.Code != http.StatusConflict {
		t.Errorf("answering twice: HTTP %d, want 409", rec.Code)
	}
	if rec := s.doJSON("POST", meBase+"/decisions/9999/answer", admin, `{"option":"drop"}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown decision: HTTP %d, want 404", rec.Code)
	}
	if open := meJSON(t, s.do("GET", meBase+"/decisions?open=1", admin), 200)["decisions"].([]any); len(open) != 0 {
		t.Errorf("answered decision still listed: %v", open)
	}

	// An item the engine already moved on: the answer is ignored with the reason.
	moved := meSeedItem(t, db, "request", "downloading", "Artist2", "Song2")
	dec2 := meSeedDecision(t, db, moved, "no_genuine_copy", "lossy", "wait", "drop")
	res = meJSON(t, s.doJSON("POST", fmt.Sprintf("%s/decisions/%d/answer", meBase, dec2), admin, `{"option":"drop"}`), 200)
	if res["ignored"] != true || res["reason"] == "" || res["state"] != "downloading" {
		t.Errorf("answer for a moved item = %v, want ignored with a reason", res)
	}
}

// A path_change decision takes only keep or drop: the journal cannot rename, so there is no
// approve.
func TestMusicEnginePathChangeOffersOnlyKeepAndDrop(t *testing.T) {
	s := newMEServer(t, &MusicEngineAPI{})
	db := s.st.DB()
	s.mountME(&MusicEngineAPI{DB: db, Decisions: meRealDecisions(t, db)})
	_, admin := s.user(t, "admin", auth.RoleAdmin)
	item := meSeedItem(t, db, "sweep", "waiting_blair", "A", "B")
	dec := meSeedDecision(t, db, item, "path_change", "approve", "keep", "drop")

	open := meJSON(t, s.do("GET", meBase+"/decisions?open=1", admin), 200)["decisions"].([]any)
	opts := open[0].(map[string]any)["options"].([]any)
	if len(opts) != 2 || opts[0] != "keep" || opts[1] != "drop" {
		t.Errorf("path_change options = %v, want [keep drop]", opts)
	}
	path := fmt.Sprintf("%s/decisions/%d/answer", meBase, dec)
	if rec := s.doJSON("POST", path, admin, `{"option":"approve"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("approve a path_change: HTTP %d, want 400", rec.Code)
	}
	if res := meJSON(t, s.doJSON("POST", path, admin, `{"option":"keep"}`), 200); res["state"] != "dropped" {
		t.Errorf("keep = %v", res)
	}
}

// remap and album_substitute decisions sit on items that are already done, so they are
// routed directly rather than through Decisions.Answer (which would ignore them).
func TestMusicEngineRemapAndAlbumDecisionsAreRoutedDirectly(t *testing.T) {
	remap := &meFakeRemapper{ran: make(chan struct{}, 4)}
	album := &meFakeAlbum{placed: make(chan musicengine.AlbumOptions, 4), confirm: make(chan int64, 4)}
	s := newMEServer(t, &MusicEngineAPI{})
	db := s.st.DB()
	s.mountME(&MusicEngineAPI{DB: db, Decisions: meRealDecisions(t, db), Remapper: remap, Album: album})
	_, admin := s.user(t, "admin", auth.RoleAdmin)
	done := meSeedItem(t, db, "request", "done", "A", "B")

	retry := meSeedDecision(t, db, done, "remap", "retry", "drop")
	res := meJSON(t, s.doJSON("POST", fmt.Sprintf("%s/decisions/%d/answer", meBase, retry), admin, `{"option":"retry"}`), 200)
	if res["ignored"] != false || res["state"] != "done" {
		t.Errorf("remap retry = %v, want not ignored", res)
	}
	select {
	case <-remap.ran:
	case <-time.After(5 * time.Second):
		t.Fatal("remap retry did not re-run the remapper")
	}
	var answered int64
	if err := db.QueryRow(`SELECT answered_at FROM mus_decisions WHERE id = ?`, retry).Scan(&answered); err != nil || answered == 0 {
		t.Errorf("remap decision not closed (answered_at %d, err %v)", answered, err)
	}

	// drop closes the question and runs nothing.
	done2 := meSeedItem(t, db, "request", "done", "C", "D")
	drop := meSeedDecision(t, db, done2, "remap", "retry", "drop")
	meJSON(t, s.doJSON("POST", fmt.Sprintf("%s/decisions/%d/answer", meBase, drop), admin, `{"option":"drop"}`), 200)
	select {
	case <-remap.ran:
		t.Error("dropping a remap decision re-ran the remapper")
	case <-time.After(100 * time.Millisecond):
	}
	if rec := s.doJSON("POST", fmt.Sprintf("%s/decisions/%d/answer", meBase, drop), admin, `{"option":"drop"}`); rec.Code != http.StatusConflict {
		t.Errorf("answering the remap decision twice: HTTP %d, want 409", rec.Code)
	}

	done3 := meSeedItem(t, db, "request", "done", "E", "F")
	alb := meSeedDecision(t, db, done3, "album_substitute", "approve", "drop")
	meJSON(t, s.doJSON("POST", fmt.Sprintf("%s/decisions/%d/answer", meBase, alb), admin, `{"option":"approve"}`), 200)
	select {
	case id := <-album.confirm:
		if id != alb {
			t.Errorf("ConfirmAlbum got decision %d, want %d", id, alb)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("approving the album decision did not call ConfirmAlbum")
	}
}

// ---- swaps and undo ----

func TestMusicEngineUndoStatusCodes(t *testing.T) {
	sw := &meFakeSwaps{}
	s := newMEServer(t, &MusicEngineAPI{Swaps: sw})
	_, admin := s.user(t, "admin", auth.RoleAdmin)

	sw.err = &musicengine.UndoRefusal{Reason: "swap 9 changed this track after swap 4; undo swap 9 first", LaterSwapID: 9}
	body := meJSON(t, s.do("POST", meBase+"/swaps/4/undo", admin), http.StatusConflict)
	if body["later_swap_id"] != float64(9) || body["error"] == "" || body["message"] == "" || body["status"] != "error" {
		t.Errorf("409 body = %v, want later_swap_id 9 and the error in both fields", body)
	}

	sw.err = &musicengine.UndoRefusal{Reason: "the old version was cleared from quarantine on 2026-10-01", Cleared: true}
	body = meJSON(t, s.do("POST", meBase+"/swaps/4/undo", admin), http.StatusGone)
	if body["error"] != "the old version was cleared from quarantine" {
		t.Errorf("410 body = %v", body)
	}

	sw.err = &musicengine.UndoRefusal{Reason: "swap 4 was already undone"}
	if body := meJSON(t, s.do("POST", meBase+"/swaps/4/undo", admin), http.StatusConflict); body["later_swap_id"] != nil {
		t.Errorf("a plain refusal carried later_swap_id: %v", body)
	}

	sw.err = errors.New("disk on fire")
	if rec := s.do("POST", meBase+"/swaps/4/undo", admin); rec.Code != http.StatusInternalServerError {
		t.Errorf("unexpected error: HTTP %d, want 500", rec.Code)
	}

	sw.err, sw.res = nil, musicengine.UndoResult{Confirmed: true}
	if body := meJSON(t, s.do("POST", meBase+"/swaps/4/undo", admin), 200); body["state"] != "restored" {
		t.Errorf("undo = %v", body)
	}
	if rec := s.do("POST", meBase+"/swaps/x/undo", admin); rec.Code != http.StatusBadRequest {
		t.Errorf("bad id: HTTP %d, want 400", rec.Code)
	}
}

// The swaps list says which swaps can be undone and why not: a later swap, a cleared
// quarantine entry, an undo already done.
func TestMusicEngineSwapsListMarksUndoable(t *testing.T) {
	s := newMEServer(t, &MusicEngineAPI{})
	db := s.st.DB()
	_, admin := s.user(t, "admin", auth.RoleAdmin)
	item := meSeedItem(t, db, "sweep", "done", "A", "B")
	q1 := meExec(t, db, `INSERT INTO mus_quarantine (path, created_at, size_bytes) VALUES ('/q/1', 1, 10)`)
	q2 := meExec(t, db, `INSERT INTO mus_quarantine (path, created_at, size_bytes) VALUES ('/q/2', 1, 10)`)
	q3 := meExec(t, db, `INSERT INTO mus_quarantine (path, created_at, size_bytes, cleared_at) VALUES ('/q/3', 1, 10, 50)`)
	cand := meExec(t, db, `INSERT INTO mus_candidates (item_id, lane, codec, bit_depth, sample_rate, verdict, created_at, pcm_fingerprint)
		VALUES (?, 'usenet', 'flac', 24, 96000, 'GENUINE', 1, 'fp-new')`, item)
	old := meExec(t, db, `INSERT INTO mus_candidates (item_id, lane, codec, bit_depth, sample_rate, verdict, created_at, pcm_fingerprint)
		VALUES (?, 'usenet', 'flac', 16, 44100, 'SUSPECT', 1, 'fp-old')`, item)
	_ = old
	ins := func(path string, quar int64, undone int64) int64 {
		return meExec(t, db, `INSERT INTO mus_swap_journal (kind, item_id, candidate_id, new_path, quarantine_id, old_pcm_fingerprint, phase, started_at, phase_at, closed_at, undone_at)
			VALUES ('swap', ?, ?, ?, ?, 'fp-old', 5, 100, 100, 101, ?)`, item, cand, path, quar, undone)
	}
	first := ins("/lib/a.flac", q1, 0)
	second := ins("/lib/a.flac", q2, 0) // a later swap of the same file
	cleared := ins("/lib/b.flac", q3, 0)
	undone := ins("/lib/c.flac", q1, 999)

	list := meJSON(t, s.do("GET", meBase+"/swaps", admin), 200)["swaps"].([]any)
	byID := map[float64]map[string]any{}
	for _, x := range list {
		m := x.(map[string]any)
		byID[m["id"].(float64)] = m
	}
	if len(byID) != 4 {
		t.Fatalf("swaps = %v", list)
	}
	check := func(id int64, undoable bool, reasonHas string) {
		t.Helper()
		m := byID[float64(id)]
		if m["undoable"] != undoable || (reasonHas != "" && !strings.Contains(fmt.Sprint(m["blocked_reason"]), reasonHas)) {
			t.Errorf("swap %d = %v, want undoable=%v reason containing %q", id, m, undoable, reasonHas)
		}
	}
	check(first, false, fmt.Sprintf("undo swap %d first", second))
	check(second, true, "")
	check(cleared, false, "cleared from quarantine")
	check(undone, false, "already undone")
	if byID[float64(undone)]["undone_at"] != float64(999) {
		t.Errorf("undone_at = %v", byID[float64(undone)]["undone_at"])
	}
	newSide := byID[float64(second)]["new"].(map[string]any)
	oldSide := byID[float64(second)]["old"].(map[string]any)
	if newSide["codec"] != "flac" || newSide["bit_depth"] != float64(24) || oldSide["verdict"] != "SUSPECT" || oldSide["bit_depth"] != float64(16) {
		t.Errorf("sides: new %v old %v", newSide, oldSide)
	}
	if since := meJSON(t, s.do("GET", meBase+"/swaps?since=200", admin), 200)["swaps"].([]any); len(since) != 0 {
		t.Errorf("since=200 returned %v, want none", since)
	}
}

// ---- full album ----

func TestMusicEngineFullAlbumShowsTheReleaseFirst(t *testing.T) {
	album := &meFakeAlbum{placed: make(chan musicengine.AlbumOptions, 2), confirm: make(chan int64, 1)}
	album.plan = musicengine.AlbumPlan{InStaging: true, Pinned: musicengine.AlbumRelease{
		Artist: "Band", Title: "The Record", Edition: "Official US 2019-03-01", TrackCount: 11}}
	s := newMEServer(t, &MusicEngineAPI{Album: album})
	db := s.st.DB()
	_, admin := s.user(t, "admin", auth.RoleAdmin)
	done := meSeedItem(t, db, "request", "done", "Band", "Hit")
	waiting := meSeedItem(t, db, "request", "searching", "Band", "Other")
	path := func(id int64) string { return fmt.Sprintf("%s/items/%d/full-album", meBase, id) }

	first := meJSON(t, s.doJSON("POST", path(done), admin, `{"confirm":false}`), 200)
	rel := first["release"].(map[string]any)
	if rel["artist"] != "Band" || rel["title"] != "The Record" || rel["track_count"] != float64(11) || rel["year"] != float64(2019) || first["needs_confirm"] != true {
		t.Errorf("first call = %v", first)
	}
	select {
	case o := <-album.placed:
		t.Fatalf("the first call placed something: %+v", o)
	case <-time.After(100 * time.Millisecond):
	}

	second := meJSON(t, s.doJSON("POST", path(done), admin, `{"confirm":true}`), 200)
	if second["needs_confirm"] != false {
		t.Errorf("confirmed call = %v", second)
	}
	select {
	case o := <-album.placed:
		if !o.Confirmed {
			t.Errorf("placed without Confirmed: %+v", o)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the confirmed call did not place the album")
	}

	if rec := s.doJSON("POST", path(waiting), admin, `{"confirm":false}`); rec.Code != http.StatusConflict {
		t.Errorf("item not yet placed: HTTP %d, want 409", rec.Code)
	}
	if rec := s.doJSON("POST", path(9999), admin, `{"confirm":false}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown item: HTTP %d, want 404", rec.Code)
	}
	album.planErr = musicengine.ErrAlbumUnavailable
	if rec := s.doJSON("POST", path(done), admin, `{"confirm":false}`); rec.Code != http.StatusConflict {
		t.Errorf("no release recorded: HTTP %d, want 409", rec.Code)
	}
	// Out of staging: the substitute is shown, and confirming says plainly it cannot fetch.
	album.planErr = nil
	album.plan = musicengine.AlbumPlan{Pinned: musicengine.AlbumRelease{Title: "Old"}, Substitute: &musicengine.AlbumRelease{Artist: "Band", Title: "Reissue", TrackCount: 13}}
	if rel := meJSON(t, s.doJSON("POST", path(done), admin, `{"confirm":false}`), 200)["release"].(map[string]any); rel["title"] != "Reissue" {
		t.Errorf("substitute not shown: %v", rel)
	}
	if rec := s.doJSON("POST", path(done), admin, `{"confirm":true}`); rec.Code != http.StatusConflict {
		t.Errorf("confirm with nothing in staging: HTTP %d, want 409", rec.Code)
	}
}

// ---- lanes, space, quarantine, alerts ----

func TestMusicEngineLanesSpaceAndQuarantine(t *testing.T) {
	q := &meFakeQuarantine{usage: musicengine.QuarantineUsage{UsedBytes: 50, ShareBytes: 100, Full: false},
		report: musicengine.ClearReport{Cleared: 3, FreedBytes: 3000}}
	sp := meFakeSpace{st: musicengine.SpaceStatus{
		Volumes:    []musicengine.SpaceVolumeStatus{{Path: "/lib", Measured: true, FreeBytes: 5}, {Path: "/dl", Measured: false}},
		FloorBytes: 100, MarginBytes: 25, IntakePaused: true, IntakeReason: "downloads is low", SweepPaused: true, SweepReason: "quarantine is full",
	}}
	s := newMEServer(t, &MusicEngineAPI{Quarantine: q, Space: sp, LibraryPath: "/lib", DownloadsPath: "/dl", Now: func() time.Time { return time.Unix(10_000_000, 0) }})
	db := s.st.DB()
	_, admin := s.user(t, "admin", auth.RoleAdmin)

	meExec(t, db, `INSERT INTO mus_lane_status (lane, status, reason, vpn_ok, checked_at, unavailable_since) VALUES ('torrent', 'unavailable', 'VPN not proven', 0, 900, 800)`)
	meExec(t, db, `INSERT INTO mus_lane_status (lane, status, vpn_ok, checked_at) VALUES ('usenet', 'available', 1, 900)`)
	lanes := meJSON(t, s.do("GET", meBase+"/lanes", admin), 200)["lanes"].([]any)
	if len(lanes) != 4 {
		t.Fatalf("lanes = %v, want all four", lanes)
	}
	by := map[string]map[string]any{}
	for _, l := range lanes {
		by[l.(map[string]any)["lane"].(string)] = l.(map[string]any)
	}
	if by["usenet"]["status"] != "available" || by["usenet"]["vpn_ok"] != true ||
		by["torrent"]["status"] != "unavailable" || by["torrent"]["reason"] != "VPN not proven" || by["torrent"]["unavailable_since"] != float64(800) ||
		by["youtube"]["status"] != "unavailable" || by["youtube"]["checked_at"] != nil {
		t.Errorf("lanes = %v", by)
	}

	space := meJSON(t, s.do("GET", meBase+"/space", admin), 200)
	vols := space["volumes"].([]any)
	v0, v1 := vols[0].(map[string]any), vols[1].(map[string]any)
	if v0["name"] != "library" || v0["measurable"] != true || v0["free_bytes"] != float64(5) || v1["name"] != "downloads" || v1["measurable"] != false {
		t.Errorf("volumes = %v", vols)
	}
	if space["floor_bytes"] != float64(100) || space["intake_paused"] != true || space["sweep_paused"] != true ||
		space["reason"] != "downloads is low; quarantine is full" || space["quarantine"].(map[string]any)["share_bytes"] != float64(100) {
		t.Errorf("space = %v", space)
	}

	// Preview counts what a clear would take: old enough, not cleared, not held by an open journal row.
	old := int64(10_000_000) - 100*24*3600
	meExec(t, db, `INSERT INTO mus_quarantine (path, created_at, size_bytes) VALUES ('/q/old1', ?, 10), ('/q/old2', ?, 20)`, old, old)
	meExec(t, db, `INSERT INTO mus_quarantine (path, created_at, size_bytes) VALUES ('/q/new', ?, 40)`, int64(10_000_000)-3600)
	meExec(t, db, `INSERT INTO mus_quarantine (path, created_at, size_bytes, cleared_at) VALUES ('/q/gone', ?, 80, 5)`, old)
	meExec(t, db, `INSERT INTO mus_quarantine (path, created_at, size_bytes) VALUES ('/q/held', ?, 160)`, old)
	meExec(t, db, `INSERT INTO mus_swap_journal (kind, phase, started_at, phase_at, quarantine_path) VALUES ('swap', 3, 1, 1, '/q/held')`)
	pv := meJSON(t, s.do("GET", meBase+"/quarantine/preview?older_than_days=90", admin), 200)
	if pv["count"] != float64(2) || pv["bytes"] != float64(30) {
		t.Errorf("preview = %v, want 2 versions, 30 bytes", pv)
	}
	if rec := s.do("GET", meBase+"/quarantine/preview?older_than_days=-1", admin); rec.Code != http.StatusBadRequest {
		t.Errorf("negative age: HTTP %d, want 400", rec.Code)
	}

	cl := meJSON(t, s.doJSON("POST", meBase+"/quarantine/clear", admin, `{"older_than_days":45}`), 200)
	if cl["removed"] != float64(3) || cl["freed_bytes"] != float64(3000) || q.clearedAge != 45*24*time.Hour {
		t.Errorf("clear = %v with age %v, want 3 removed and 45 days", cl, q.clearedAge)
	}
	meJSON(t, s.doJSON("POST", meBase+"/quarantine/clear", admin, `{}`), 200)
	if q.clearedAge != musicengine.DefaultQuarantineClearAge {
		t.Errorf("default age = %v, want %v", q.clearedAge, musicengine.DefaultQuarantineClearAge)
	}
	q.err = errors.New("cannot remove /q/x")
	body := meJSON(t, s.doJSON("POST", meBase+"/quarantine/clear", admin, `{"older_than_days":1}`), http.StatusInternalServerError)
	if !strings.Contains(fmt.Sprint(body["error"]), "cannot remove") || body["removed"] != float64(3) {
		t.Errorf("partial failure body = %v, want the cause and how many were removed", body)
	}
}

func TestMusicEngineAlertFailuresListsOnlyEngineDeliveryFailures(t *testing.T) {
	s := newMEServer(t, &MusicEngineAPI{})
	db := s.st.DB()
	_, admin := s.user(t, "admin", auth.RoleAdmin)
	conn := meExec(t, db, `INSERT INTO notifications (name, kind, url) VALUES ('Telegram', 'apprise', 'tgram://secret-token/chat')`)
	ins := func(event, status string, attempts int, lastErr string) {
		meExec(t, db, `INSERT INTO notification_deliveries (connection_id, event_key, status, attempts, last_error, created_at) VALUES (?, ?, ?, ?, ?, 777)`,
			conn, event, status, attempts, lastErr)
	}
	ins("musicengine.item_past_deadline", "failed", 4, "telegram: 401")
	ins("musicengine.daily_digest", "queued", 2, "timeout")
	ins("musicengine.daily_digest", "sent", 1, "")
	ins("musicengine.lane_unavailable", "queued", 0, "")
	ins("movie.grabbed", "failed", 4, "other event")

	rec := s.do("GET", meBase+"/alerts/failures", admin)
	if strings.Contains(rec.Body.String(), "secret-token") {
		t.Fatalf("the connection URL leaked: %s", rec.Body)
	}
	fails := meJSON(t, rec, 200)["failures"].([]any)
	if len(fails) != 2 {
		t.Fatalf("failures = %v, want the failed alert and the retrying one", fails)
	}
	f := fails[1].(map[string]any)
	if f["event"] != "musicengine.item_past_deadline" || f["connection"] != "Telegram" || f["last_error"] != "telegram: 401" || f["attempts"] != float64(4) || f["updated_at"] != float64(777) {
		t.Errorf("failure = %v", f)
	}
}
