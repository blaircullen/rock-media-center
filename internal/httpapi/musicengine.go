package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tristenlammi/arrmada/internal/audiocheck"
	"github.com/tristenlammi/arrmada/internal/auth"
	"github.com/tristenlammi/arrmada/internal/jobs"
	"github.com/tristenlammi/arrmada/internal/musicengine"
	"github.com/tristenlammi/arrmada/internal/settings"
	"github.com/tristenlammi/arrmada/internal/spotify"
)

// The lossless music engine's admin API (plan U13; contract in the plan's KTD12/KTD16 and
// docs for the status page, web/src/lib/musicEngineApi.ts). Every route is staff-only,
// as the Music module's are, except the heartbeat: the watchdog on the Beast host reads it
// from the LAN with no session, and it says nothing but a timestamp. The heartbeat is not
// marked ext(), so it is closed to the internet like every other engine route.
//
// Failures are written in the repo's envelope ({"status":"error","message":…}) with the
// same sentence also under "error", which is what the status page reads.

// Setting keys. The engine's own settings live under one prefix; U15 reads them through
// LoadMusicEngineSettings and MusicEngineAutoSwaps rather than by key.
const (
	MusicEngineKeyPlaylist  = "musicengine_watched_playlist"
	MusicEngineKeyFloorGB   = "musicengine_floor_gb"
	MusicEngineKeyMarginGB  = "musicengine_margin_gb"
	MusicEngineKeyQuarGB    = "musicengine_quarantine_share_gb"
	MusicEngineKeyAutoSwaps = "musicengine_auto_swaps"
	MusicEngineKeyEnabled   = "musicengine_enabled"
	// The two release checks (R13, KTD16) are stored separately as JSON.
	MusicEngineKeyCalibration = "musicengine_check_calibration"
	MusicEngineKeyPathTest    = "musicengine_check_path_test"
)

// Defaults: the playlist from R1, the space figures from KTD10 and R34. The engine ships
// switched off; the first intake run is a deliberate step (plan, U16).
const (
	meDefaultPlaylist = "5hLOEmkXHaXQDDbvoFFLX8"
	meDefaultFloorGB  = 100.0
	meDefaultMarginGB = 25.0
	meDefaultQuarGB   = 200.0

	meMaxCalibrationRows = 50
	meMaxItemsPage       = 500
	meDefaultItemsPage   = 100
	meMaxSwaps           = 200
	meMaxClearDays       = 3650
)

const meAutoSwapsRefusal = "automatic swaps stay off until both release checks pass"

// ---- what the routes call (satisfied by the musicengine services; fakes in tests) ----

// MusicEngineDecisions is musicengine.Decisions.
type MusicEngineDecisions interface {
	Get(ctx context.Context, id int64) (musicengine.Decision, error)
	Open(ctx context.Context) ([]musicengine.Decision, error)
	Answer(ctx context.Context, decisionID int64, option, note string) (musicengine.AnswerResult, error)
}

// MusicEngineSwaps is musicengine.Swaps.
type MusicEngineSwaps interface {
	Undo(ctx context.Context, swapID int64) (musicengine.UndoResult, error)
}

// MusicEngineQuarantine is musicengine.Quarantine.
type MusicEngineQuarantine interface {
	Clear(ctx context.Context, olderThan time.Duration) (musicengine.ClearReport, error)
	Usage(ctx context.Context) (musicengine.QuarantineUsage, error)
}

// MusicEngineSpace is musicengine.Space.
type MusicEngineSpace interface {
	Status(ctx context.Context) (musicengine.SpaceStatus, error)
}

// MusicEngineAlbum is musicengine.Album.
type MusicEngineAlbum interface {
	Plan(ctx context.Context, itemID int64) (musicengine.AlbumPlan, error)
	Place(ctx context.Context, itemID int64, opts musicengine.AlbumOptions) (musicengine.AlbumResult, error)
	ConfirmAlbum(ctx context.Context, decisionID int64, stagingDir string) (musicengine.AlbumResult, error)
}

// MusicEngineRemapper is musicengine.Remapper.
type MusicEngineRemapper interface {
	Remap(ctx context.Context) (musicengine.RemapReport, error)
}

// MusicEngineHeartbeat is musicengine.Heartbeat.
type MusicEngineHeartbeat interface {
	Status() musicengine.HeartbeatStatus
}

// MusicEngineChecker is audiocheck.Checker (the calibration run).
type MusicEngineChecker interface {
	Check(ctx context.Context, path string) audiocheck.Result
}

// MusicEngineAPI is everything the engine routes need. U15 builds it from the engine's
// services and passes it to registerMusicEngineRoutes. A nil field makes the routes that
// need it answer 503 ("not wired"); the rest keep working.
type MusicEngineAPI struct {
	// DB is the engine's database. Item, swap, lane and delivery listings are read-only
	// queries over the mus_* tables and the notify delivery log; every write goes through
	// the services below.
	DB         *sql.DB
	Decisions  MusicEngineDecisions
	Swaps      MusicEngineSwaps
	Quarantine MusicEngineQuarantine
	Space      MusicEngineSpace
	Album      MusicEngineAlbum
	Remapper   MusicEngineRemapper
	Heartbeat  MusicEngineHeartbeat
	Checker    MusicEngineChecker

	// LibraryRoot is the music library root (/mnt/nas/music); calibration paths must sit
	// under it. LibraryPath and DownloadsPath name the volumes the space gate measures.
	LibraryRoot   func() string
	LibraryPath   string
	DownloadsPath string

	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu    sync.Mutex // settings and release-check writes
	calMu sync.Mutex // one calibration run at a time
	qMu   sync.Mutex // one quarantine clear at a time
}

func (m *MusicEngineAPI) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// MusicEngineSettings is the engine's own settings as the status page edits them.
type MusicEngineSettings struct {
	WatchedPlaylist   string  `json:"watched_playlist"`
	FloorGB           float64 `json:"floor_gb"`
	MarginGB          float64 `json:"margin_gb"`
	QuarantineShareGB float64 `json:"quarantine_share_gb"`
	AutoSwaps         bool    `json:"auto_swaps"`
	Enabled           bool    `json:"enabled"`
}

// LoadMusicEngineSettings reads the stored settings, with defaults for anything unset.
// AutoSwaps is the stored flag only; MusicEngineAutoSwaps adds the release-check rule.
func LoadMusicEngineSettings(ctx context.Context, s *settings.Service) MusicEngineSettings {
	num := func(key string, def float64) float64 {
		raw := s.Get(ctx, key, "")
		if raw == "" {
			return def
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return def
		}
		return v
	}
	return MusicEngineSettings{
		WatchedPlaylist:   s.Get(ctx, MusicEngineKeyPlaylist, meDefaultPlaylist),
		FloorGB:           num(MusicEngineKeyFloorGB, meDefaultFloorGB),
		MarginGB:          num(MusicEngineKeyMarginGB, meDefaultMarginGB),
		QuarantineShareGB: num(MusicEngineKeyQuarGB, meDefaultQuarGB),
		AutoSwaps:         s.GetBool(ctx, MusicEngineKeyAutoSwaps, false),
		Enabled:           s.GetBool(ctx, MusicEngineKeyEnabled, false),
	}
}

// MusicEngineAutoSwaps is the KTD16 rule for the sweep: automatic swaps run only when the
// setting is on AND both release checks are recorded as passed. U15 passes this as
// SwapsConfig.AutoSwaps, so a hand-edited setting cannot bypass the checks.
func MusicEngineAutoSwaps(ctx context.Context, s *settings.Service) bool {
	if !s.GetBool(ctx, MusicEngineKeyAutoSwaps, false) {
		return false
	}
	return meCheckPassed(ctx, s, MusicEngineKeyCalibration, nil) && meCheckPassed(ctx, s, MusicEngineKeyPathTest, nil)
}

// meCheck is one stored release check.
type meCheck struct {
	Passed  *bool  `json:"passed"`
	At      *int64 `json:"at"`
	Summary string `json:"summary"`
}

func meLoadCheck(ctx context.Context, s *settings.Service, key string, log func(string, ...any)) meCheck {
	raw := s.Get(ctx, key, "")
	if raw == "" {
		return meCheck{}
	}
	var c meCheck
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		// An unreadable record is "not passed", never "passed".
		if log != nil {
			log("musicengine: stored release check is unreadable, treating it as not run", "key", key, "err", err)
		}
		return meCheck{}
	}
	return c
}

func meCheckPassed(ctx context.Context, s *settings.Service, key string, log func(string, ...any)) bool {
	c := meLoadCheck(ctx, s, key, log)
	return c.Passed != nil && *c.Passed
}

// ---- routes ----

// registerMusicEngineRoutes adds the engine's routes. U15 calls it from registerRoutes with
// the services it built; m may be nil (engine not built), in which case every route
// answers 503 so the route table, and the golden, stay the same either way.
func (a *api) registerMusicEngineRoutes(mux *router, m *MusicEngineAPI) {
	const base = "/api/v1/musicengine"
	staff := func(h func(w http.ResponseWriter, r *http.Request, m *MusicEngineAPI)) guard {
		return a.requireRole(auth.RoleManager, a.meRoute(m, h))
	}
	mux.HandleFunc("GET "+base+"/items", staff(a.meListItems))
	mux.HandleFunc("GET "+base+"/items/{id}", staff(a.meGetItem))
	mux.HandleFunc("POST "+base+"/items/{id}/full-album", staff(a.meFullAlbum))
	mux.HandleFunc("GET "+base+"/decisions", staff(a.meListDecisions))
	mux.HandleFunc("POST "+base+"/decisions/{id}/answer", staff(a.meAnswerDecision))
	mux.HandleFunc("GET "+base+"/swaps", staff(a.meListSwaps))
	mux.HandleFunc("POST "+base+"/swaps/{id}/undo", staff(a.meUndoSwap))
	mux.HandleFunc("GET "+base+"/lanes", staff(a.meLanes))
	mux.HandleFunc("GET "+base+"/space", staff(a.meSpace))
	mux.HandleFunc("GET "+base+"/quarantine/preview", staff(a.meQuarantinePreview))
	mux.HandleFunc("POST "+base+"/quarantine/clear", staff(a.meQuarantineClear))
	mux.HandleFunc("GET "+base+"/alerts/failures", staff(a.meAlertFailures))
	mux.HandleFunc("GET "+base+"/settings", staff(a.meGetSettings))
	mux.HandleFunc("PUT "+base+"/settings", staff(a.mePutSettings))
	mux.HandleFunc("GET "+base+"/release-checks", staff(a.meGetReleaseChecks))
	mux.HandleFunc("POST "+base+"/release-checks/calibration", staff(a.meCalibration))
	mux.HandleFunc("POST "+base+"/release-checks/path-test", staff(a.mePathTest))
	// No session: the watchdog reads this from the LAN. Public, and deliberately not ext().
	mux.HandleFunc("GET "+base+"/heartbeat", a.public(a.meRoute(m, a.meHeartbeat)))
}

// meRoute turns a handler that needs the engine into a plain one, answering 503 when the
// engine is not built.
func (a *api) meRoute(m *MusicEngineAPI, h func(w http.ResponseWriter, r *http.Request, m *MusicEngineAPI)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if m == nil {
			a.meError(w, http.StatusServiceUnavailable, "the music engine is not running")
			return
		}
		h(w, r, m)
	}
}

// meError is writeError's envelope plus the same sentence under "error", with any extra
// fields (later_swap_id) alongside.
func (a *api) meError(w http.ResponseWriter, status int, msg string, extra ...any) {
	body := map[string]any{"status": "error", "message": msg, "error": msg}
	for i := 0; i+1 < len(extra); i += 2 {
		if k, ok := extra[i].(string); ok {
			body[k] = extra[i+1]
		}
	}
	a.writeJSON(w, status, body)
}

// meFail logs the real error and answers 500; the log line is the evidence.
func (a *api) meFail(w http.ResponseWriter, r *http.Request, what string, err error) {
	a.deps.Log.Error("musicengine api: "+what, "method", r.Method, "path", r.URL.Path, "err", err)
	a.meError(w, http.StatusInternalServerError, "could not "+what+": "+err.Error())
}

func meNullable(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

func meNeed(a *api, w http.ResponseWriter, ok bool, what string) bool {
	if !ok {
		a.meError(w, http.StatusServiceUnavailable, what+" is not wired in this build")
	}
	return ok
}

// ---- items ----

type meItem struct {
	ID            int64  `json:"id"`
	Kind          string `json:"kind"`
	State         string `json:"state"`
	LastReason    string `json:"last_reason"`
	EnteredAt     int64  `json:"entered_at"`
	DeadlineAt    *int64 `json:"deadline_at"`
	Priority      int    `json:"priority"`
	Title         string `json:"title"`
	Artist        string `json:"artist"`
	Album         string `json:"album"`
	RecordingMBID string `json:"recording_mbid"`
	TrackPath     string `json:"track_path"`
}

const meItemSelect = `
SELECT i.id, i.kind, i.state, i.last_reason, i.entered_at, i.deadline_at, i.priority,
       COALESCE(NULLIF(r.title, ''),  NULLIF(e.title, ''),  t.title,  ''),
       COALESCE(NULLIF(r.artist, ''), NULLIF(e.artist, ''), t.artist, ''),
       COALESCE(NULLIF(e.album, ''),  t.album, ''),
       COALESCE(r.mbid, ''), COALESCE(t.path, '')
FROM mus_items i
LEFT JOIN mus_recordings r ON r.id = i.recording_id
LEFT JOIN mus_library_tracks t ON t.id = i.track_id
LEFT JOIN mus_playlist_entries e ON e.id = (SELECT MIN(id) FROM mus_playlist_entries WHERE item_id = i.id)`

func meScanItem(r interface{ Scan(...any) error }) (meItem, error) {
	var it meItem
	var kind string
	var deadline int64
	if err := r.Scan(&it.ID, &kind, &it.State, &it.LastReason, &it.EnteredAt, &deadline, &it.Priority,
		&it.Title, &it.Artist, &it.Album, &it.RecordingMBID, &it.TrackPath); err != nil {
		return it, err
	}
	// The page knows two kinds; an "upgrade" is a request whose library copy was flawed.
	it.Kind = "request"
	if kind == musicengine.KindSweep {
		it.Kind = "sweep"
	}
	it.DeadlineAt = meNullable(deadline)
	return it, nil
}

var meStateRE = regexp.MustCompile(`^[a-z_]{1,40}$`)

func (a *api) meListItems(w http.ResponseWriter, r *http.Request, m *MusicEngineAPI) {
	if !meNeed(a, w, m.DB != nil, "the engine database") {
		return
	}
	q := r.URL.Query()
	state := q.Get("state")
	if state != "" && !meStateRE.MatchString(state) {
		a.meError(w, http.StatusBadRequest, "state is not a known item state")
		return
	}
	limit := meDefaultItemsPage
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			a.meError(w, http.StatusBadRequest, "limit must be a positive number")
			return
		}
		limit = min(n, meMaxItemsPage)
	}
	var before int64
	if s := q.Get("before_id"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 1 {
			a.meError(w, http.StatusBadRequest, "before_id must be an item id")
			return
		}
		before = n
	}
	rows, err := m.DB.QueryContext(r.Context(),
		meItemSelect+` WHERE (? = '' OR i.state = ?) AND (? = 0 OR i.id < ?) ORDER BY i.id DESC LIMIT ?`,
		state, state, before, before, limit+1)
	if err != nil {
		a.meFail(w, r, "list the items", err)
		return
	}
	defer rows.Close()
	items := []meItem{}
	for rows.Next() {
		it, err := meScanItem(rows)
		if err != nil {
			a.meFail(w, r, "read an item", err)
			return
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		a.meFail(w, r, "list the items", err)
		return
	}
	var next *int64
	if len(items) > limit {
		items = items[:limit]
		next = &items[limit-1].ID
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_before_id": next})
}

type meCandidate struct {
	ID           int64  `json:"id"`
	Lane         string `json:"lane"`
	State        string `json:"state"`
	ReleaseName  string `json:"release_name"`
	Verdict      string `json:"verdict"`
	RejectReason string `json:"reject_reason"`
	Codec        string `json:"codec"`
	BitDepth     int    `json:"bit_depth"`
	SampleRate   int    `json:"sample_rate"`
	Bitrate      int    `json:"bitrate"`
	SizeBytes    int64  `json:"size_bytes"`
	CheckedAt    *int64 `json:"checked_at"`
}

type meJournal struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	Phase     int    `json:"phase"`
	StartedAt int64  `json:"started_at"`
	ClosedAt  *int64 `json:"closed_at"`
	LastError string `json:"last_error"`
}

func (a *api) meGetItem(w http.ResponseWriter, r *http.Request, m *MusicEngineAPI) {
	if !meNeed(a, w, m.DB != nil, "the engine database") {
		return
	}
	id, ok := a.pathID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	it, err := meScanItem(m.DB.QueryRowContext(ctx, meItemSelect+` WHERE i.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		a.meError(w, http.StatusNotFound, "that song is not in the engine")
		return
	}
	if err != nil {
		a.meFail(w, r, "read the item", err)
		return
	}

	cands := []meCandidate{}
	crow, err := m.DB.QueryContext(ctx,
		`SELECT id, lane, state, release_name, verdict, reject_reason, codec, bit_depth, sample_rate, bitrate, size_bytes, checked_at
		 FROM mus_candidates WHERE item_id = ? ORDER BY id`, id)
	if err != nil {
		a.meFail(w, r, "read the item's candidates", err)
		return
	}
	defer crow.Close()
	for crow.Next() {
		var c meCandidate
		var checked int64
		if err := crow.Scan(&c.ID, &c.Lane, &c.State, &c.ReleaseName, &c.Verdict, &c.RejectReason, &c.Codec,
			&c.BitDepth, &c.SampleRate, &c.Bitrate, &c.SizeBytes, &checked); err != nil {
			a.meFail(w, r, "read a candidate", err)
			return
		}
		c.CheckedAt = meNullable(checked)
		cands = append(cands, c)
	}
	if err := crow.Err(); err != nil {
		a.meFail(w, r, "read the item's candidates", err)
		return
	}

	journal := []meJournal{}
	jrow, err := m.DB.QueryContext(ctx,
		`SELECT id, kind, phase, started_at, closed_at, last_error FROM mus_swap_journal WHERE item_id = ? ORDER BY id`, id)
	if err != nil {
		a.meFail(w, r, "read the item's journal", err)
		return
	}
	defer jrow.Close()
	for jrow.Next() {
		var j meJournal
		var closed int64
		if err := jrow.Scan(&j.ID, &j.Kind, &j.Phase, &j.StartedAt, &closed, &j.LastError); err != nil {
			a.meFail(w, r, "read a journal row", err)
			return
		}
		j.ClosedAt = meNullable(closed)
		journal = append(journal, j)
	}
	if err := jrow.Err(); err != nil {
		a.meFail(w, r, "read the item's journal", err)
		return
	}

	decs, err := m.decisionsForItem(ctx, id)
	if err != nil {
		a.meFail(w, r, "read the item's decisions", err)
		return
	}
	views := make([]meDecision, 0, len(decs))
	for _, d := range decs {
		views = append(views, meDecisionView(d, it))
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"id": it.ID, "kind": it.Kind, "state": it.State, "last_reason": it.LastReason,
		"entered_at": it.EnteredAt, "deadline_at": it.DeadlineAt, "priority": it.Priority,
		"title": it.Title, "artist": it.Artist, "album": it.Album,
		"recording_mbid": it.RecordingMBID, "track_path": it.TrackPath,
		"candidates": cands, "journal": journal, "decisions": views,
	})
}

// ---- decisions ----

type meDecisionItem struct {
	Title  string `json:"title"`
	Artist string `json:"artist"`
	Album  string `json:"album"`
	State  string `json:"state"`
}

type meDecision struct {
	ID         int64          `json:"id"`
	ItemID     int64          `json:"item_id"`
	Kind       string         `json:"kind"`
	Reason     string         `json:"reason"`
	Options    []string       `json:"options"`
	CreatedAt  int64          `json:"created_at"`
	Answer     *string        `json:"answer"`
	AnsweredAt *int64         `json:"answered_at"`
	Item       meDecisionItem `json:"item"`
}

// mePathChangeOptions are the only answers a path_change decision takes: the journal can
// rename nothing, so there is no "approve" (wiring notes, U11).
func meOptionsFor(kind string, opts []string) []string {
	out := make([]string, 0, len(opts))
	for _, o := range opts {
		if kind == musicengine.DecisionPathChange && o != musicengine.OptionKeep && o != musicengine.OptionDrop {
			continue
		}
		out = append(out, o)
	}
	return out
}

func meDecisionView(d musicengine.Decision, it meItem) meDecision {
	v := meDecision{
		ID: d.ID, ItemID: d.ItemID, Kind: d.Kind, Reason: d.Reason,
		Options: meOptionsFor(d.Kind, d.Options), CreatedAt: d.CreatedAt,
		AnsweredAt: meNullable(d.AnsweredAt),
		Item:       meDecisionItem{Title: it.Title, Artist: it.Artist, Album: it.Album, State: it.State},
	}
	if d.AnsweredAt != 0 {
		ans := d.Answer
		v.Answer = &ans
	}
	return v
}

// decisionsForItem reads every decision (open or answered) on one item, oldest first.
func (m *MusicEngineAPI) decisionsForItem(ctx context.Context, itemID int64) ([]musicengine.Decision, error) {
	rows, err := m.DB.QueryContext(ctx,
		`SELECT id, item_id, kind, reason, options, answer, answer_note, created_at, answered_at
		 FROM mus_decisions WHERE item_id = ? ORDER BY id`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []musicengine.Decision
	for rows.Next() {
		var d musicengine.Decision
		var opts string
		if err := rows.Scan(&d.ID, &d.ItemID, &d.Kind, &d.Reason, &opts, &d.Answer, &d.AnswerNote, &d.CreatedAt, &d.AnsweredAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(opts), &d.Options); err != nil {
			return nil, fmt.Errorf("decision %d has unreadable options %q: %w", d.ID, opts, err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (m *MusicEngineAPI) itemInfo(ctx context.Context, id int64) (meItem, error) {
	return meScanItem(m.DB.QueryRowContext(ctx, meItemSelect+` WHERE i.id = ?`, id))
}

func (a *api) meListDecisions(w http.ResponseWriter, r *http.Request, m *MusicEngineAPI) {
	if !meNeed(a, w, m.Decisions != nil && m.DB != nil, "the decision list") {
		return
	}
	// Only the open list is served: answered decisions are history, shown on the item.
	list, err := m.Decisions.Open(r.Context())
	if err != nil {
		a.meFail(w, r, "list the decisions", err)
		return
	}
	out := make([]meDecision, 0, len(list))
	for _, d := range list {
		it, err := m.itemInfo(r.Context(), d.ItemID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			a.meFail(w, r, "read a decision's song", err)
			return
		}
		out = append(out, meDecisionView(d, it))
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"decisions": out})
}

func (a *api) meAnswerDecision(w http.ResponseWriter, r *http.Request, m *MusicEngineAPI) {
	if !meNeed(a, w, m.Decisions != nil && m.DB != nil, "the decision list") {
		return
	}
	id, ok := a.pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		Option string `json:"option"`
		Note   string `json:"note"`
	}
	if !a.decodeJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	d, err := m.Decisions.Get(ctx, id)
	switch {
	case errors.Is(err, musicengine.ErrDecisionNotFound):
		a.meError(w, http.StatusNotFound, "that decision does not exist")
		return
	case err != nil:
		a.meFail(w, r, "read the decision", err)
		return
	}
	if d.AnsweredAt != 0 {
		a.meError(w, http.StatusConflict, "that decision was already answered")
		return
	}
	if !meHasOption(meOptionsFor(d.Kind, d.Options), req.Option) {
		a.meError(w, http.StatusBadRequest, "that is not one of this decision's choices")
		return
	}

	switch d.Kind {
	case "remap":
		// Raised on an item that is already done, so Decisions.Answer would ignore it
		// (wiring notes): retry re-runs the remap, drop closes the question.
		m.answerOutOfBand(w, r, a, d, req.Option, req.Note, func() (func(context.Context) error, bool) {
			if req.Option != musicengine.OptionRetry {
				return nil, true
			}
			if !meNeed(a, w, m.Remapper != nil, "the remapper") {
				return nil, false
			}
			return func(ctx context.Context) error { _, err := m.Remapper.Remap(ctx); return err }, true
		})
		return
	case musicengine.DecisionAlbumSubstitute:
		m.answerOutOfBand(w, r, a, d, req.Option, req.Note, func() (func(context.Context) error, bool) {
			if req.Option != musicengine.OptionApprove {
				return nil, true
			}
			if !meNeed(a, w, m.Album != nil, "the album placer") {
				return nil, false
			}
			// ConfirmAlbum closes the decision itself once placement ran.
			return func(ctx context.Context) error { _, err := m.Album.ConfirmAlbum(ctx, d.ID, ""); return err }, true
		})
		return
	}

	res, err := m.Decisions.Answer(ctx, id, req.Option, req.Note)
	switch {
	case errors.Is(err, musicengine.ErrDecisionNotFound):
		a.meError(w, http.StatusNotFound, "that decision does not exist")
	case errors.Is(err, musicengine.ErrDecisionAnswered):
		a.meError(w, http.StatusConflict, "that decision was already answered")
	case errors.Is(err, musicengine.ErrInvalidOption):
		a.meError(w, http.StatusBadRequest, "that is not one of this decision's choices")
	case errors.Is(err, musicengine.ErrNoAnswerHook):
		a.deps.Log.Warn("musicengine api: answer has no handler wired", "decision", id, "option", req.Option, "err", err)
		a.meError(w, http.StatusNotImplemented, "that answer is not available yet")
	case err != nil:
		a.meFail(w, r, "apply that answer", err)
	default:
		out := map[string]any{"ignored": res.Ignored, "state": string(res.State)}
		if res.Ignored {
			out["reason"] = res.Reason
		}
		a.writeJSON(w, http.StatusOK, out)
	}
}

func meHasOption(opts []string, o string) bool {
	for _, x := range opts {
		if x == o {
			return true
		}
	}
	return false
}

// answerOutOfBand answers a decision whose item is not waiting_blair. plan returns the
// background work the answer stands for (nil = just close the decision) and false when it
// already wrote an error. A decision that closes itself after its work (the album) is
// left open here; every other is closed before the work starts so a double click cannot
// run it twice.
func (m *MusicEngineAPI) answerOutOfBand(w http.ResponseWriter, r *http.Request, a *api, d musicengine.Decision, option, note string,
	plan func() (func(context.Context) error, bool)) {
	work, ok := plan()
	if !ok {
		return
	}
	ctx := r.Context()
	if d.Kind == musicengine.DecisionAlbumSubstitute && work != nil {
		_, existing, ok := a.submitOr503(w, r, jobs.Spec{
			Kind: "musicengine.album", Target: "decision:" + strconv.FormatInt(d.ID, 10), Class: "musicengine.album",
			Timeout: 2 * time.Hour,
			Fn: func(ctx context.Context, _ *jobs.Progress) (any, error) {
				if err := work(ctx); err != nil {
					a.deps.Log.Error("musicengine api: placing the album failed", "decision", d.ID, "err", err)
					return nil, err
				}
				return nil, nil
			}})
		if !ok {
			return
		}
		if existing {
			a.meError(w, http.StatusConflict, "that album is already being placed")
			return
		}
	} else {
		if err := m.closeDecision(ctx, d.ID, option, note); err != nil {
			if errors.Is(err, musicengine.ErrDecisionAnswered) {
				a.meError(w, http.StatusConflict, "that decision was already answered")
				return
			}
			a.meFail(w, r, "record the answer", err)
			return
		}
		if work != nil {
			_, _, ok := a.submitOr503(w, r, jobs.Spec{
				Kind: "musicengine." + d.Kind, Target: "decision:" + strconv.FormatInt(d.ID, 10), Class: "musicengine.remap",
				Timeout: 2 * time.Hour,
				Fn: func(ctx context.Context, _ *jobs.Progress) (any, error) {
					if err := work(ctx); err != nil {
						a.deps.Log.Error("musicengine api: background work for an answer failed", "decision", d.ID, "kind", d.Kind, "err", err)
						return nil, err
					}
					return nil, nil
				}})
			if !ok {
				return
			}
		}
	}
	state := ""
	if it, err := m.itemInfo(ctx, d.ItemID); err == nil {
		state = it.State
	} else if !errors.Is(err, sql.ErrNoRows) {
		a.deps.Log.Warn("musicengine api: could not read the item after an answer", "item", d.ItemID, "err", err)
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"ignored": false, "state": state})
}

// closeDecision records an answer for the kinds that do not go through Decisions.Answer.
func (m *MusicEngineAPI) closeDecision(ctx context.Context, id int64, option, note string) error {
	if len(note) > 500 {
		note = note[:500]
	}
	res, err := m.DB.ExecContext(ctx,
		`UPDATE mus_decisions SET answer = ?, answer_note = ?, answered_at = ? WHERE id = ? AND answered_at = 0`,
		option, note, m.now().Unix(), id)
	if err != nil {
		return fmt.Errorf("recording the answer to decision %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("%w: %d", musicengine.ErrDecisionAnswered, id)
	}
	return nil
}

// ---- swaps and undo ----

type meSide struct {
	Codec      string `json:"codec"`
	BitDepth   int    `json:"bit_depth"`
	SampleRate int    `json:"sample_rate"`
	Bitrate    int    `json:"bitrate"`
	Verdict    string `json:"verdict"`
}

type meSwap struct {
	ID            int64  `json:"id"`
	ItemID        int64  `json:"item_id"`
	TrackPath     string `json:"track_path"`
	CreatedAt     int64  `json:"created_at"`
	UndoneAt      *int64 `json:"undone_at"`
	Undoable      bool   `json:"undoable"`
	BlockedReason string `json:"blocked_reason,omitempty"`
	Old           meSide `json:"old"`
	New           meSide `json:"new"`
}

func (a *api) meListSwaps(w http.ResponseWriter, r *http.Request, m *MusicEngineAPI) {
	if !meNeed(a, w, m.DB != nil, "the engine database") {
		return
	}
	var since int64
	if s := r.URL.Query().Get("since"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 {
			a.meError(w, http.StatusBadRequest, "since must be a unix time")
			return
		}
		since = n
	}
	// Completed swaps only. The old side is the engine's record of the candidate whose
	// audio fingerprint matches what the swap replaced; a file the engine never placed
	// itself has no such record, and its side is left empty.
	rows, err := m.DB.QueryContext(r.Context(), `
SELECT j.id, COALESCE(j.item_id, 0), j.new_path, j.started_at, j.undone_at,
       j.quarantine_id IS NOT NULL, COALESCE(q.cleared_at, 0),
       COALESCE((SELECT MIN(l.id) FROM mus_swap_journal l
                 WHERE l.kind = ? AND l.id > j.id AND (l.track_id = j.track_id OR l.new_path = j.new_path)
                   AND l.undone_at = 0 AND (l.phase >= ? OR l.closed_at = 0)), 0),
       COALESCE(n.codec, ''), COALESCE(n.bit_depth, 0), COALESCE(n.sample_rate, 0), COALESCE(n.bitrate, 0), COALESCE(n.verdict, ''),
       COALESCE(o.codec, ''), COALESCE(o.bit_depth, 0), COALESCE(o.sample_rate, 0), COALESCE(o.bitrate, 0), COALESCE(o.verdict, '')
FROM mus_swap_journal j
LEFT JOIN mus_quarantine q ON q.id = j.quarantine_id
LEFT JOIN mus_candidates n ON n.id = j.candidate_id
LEFT JOIN mus_candidates o ON o.id = CASE WHEN j.old_pcm_fingerprint = '' THEN NULL
        ELSE (SELECT MAX(id) FROM mus_candidates WHERE pcm_fingerprint = j.old_pcm_fingerprint) END
WHERE j.kind = ? AND j.phase >= ? AND j.started_at >= ?
ORDER BY j.id DESC LIMIT ?`,
		musicengine.JournalSwap, musicengine.PhasePlexConfirmed, musicengine.JournalSwap, musicengine.PhasePlexConfirmed, since, meMaxSwaps)
	if err != nil {
		a.meFail(w, r, "list the swaps", err)
		return
	}
	defer rows.Close()
	out := []meSwap{}
	for rows.Next() {
		var s meSwap
		var undone, cleared, later int64
		var hasQuarantine bool
		if err := rows.Scan(&s.ID, &s.ItemID, &s.TrackPath, &s.CreatedAt, &undone, &hasQuarantine, &cleared, &later,
			&s.New.Codec, &s.New.BitDepth, &s.New.SampleRate, &s.New.Bitrate, &s.New.Verdict,
			&s.Old.Codec, &s.Old.BitDepth, &s.Old.SampleRate, &s.Old.Bitrate, &s.Old.Verdict); err != nil {
			a.meFail(w, r, "read a swap", err)
			return
		}
		s.UndoneAt = meNullable(undone)
		switch {
		case undone != 0:
			s.BlockedReason = "this swap was already undone"
		case later != 0:
			s.BlockedReason = fmt.Sprintf("swap %d changed this track after this one; undo swap %d first", later, later)
		case !hasQuarantine:
			s.BlockedReason = "the old version was not kept"
		case cleared != 0:
			s.BlockedReason = "the old version was cleared from quarantine"
		default:
			s.Undoable = true
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		a.meFail(w, r, "list the swaps", err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"swaps": out})
}

func (a *api) meUndoSwap(w http.ResponseWriter, r *http.Request, m *MusicEngineAPI) {
	if !meNeed(a, w, m.Swaps != nil, "swap undo") {
		return
	}
	id, ok := a.pathID(w, r)
	if !ok {
		return
	}
	res, err := m.Swaps.Undo(r.Context(), id)
	var refusal *musicengine.UndoRefusal
	switch {
	case errors.As(err, &refusal):
		switch {
		case refusal.Cleared:
			a.meError(w, http.StatusGone, "the old version was cleared from quarantine")
		case refusal.LaterSwapID != 0:
			a.meError(w, http.StatusConflict, refusal.Reason, "later_swap_id", refusal.LaterSwapID)
		default:
			a.meError(w, http.StatusConflict, refusal.Reason)
		}
	case err != nil:
		a.meFail(w, r, "undo the swap", err)
	default:
		state := "restored"
		if !res.Confirmed {
			// The file is back; Plex has not listed it yet and the sweep finishes the read-back.
			state = "restored_waiting_for_plex"
		}
		a.writeJSON(w, http.StatusOK, map[string]any{"state": state})
	}
}

// ---- full album ----

var meYearRE = regexp.MustCompile(`\b(19|20)\d{2}\b`)

func (a *api) meFullAlbum(w http.ResponseWriter, r *http.Request, m *MusicEngineAPI) {
	if !meNeed(a, w, m.Album != nil && m.DB != nil, "the album placer") {
		return
	}
	id, ok := a.pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		Confirm bool `json:"confirm"`
	}
	if !a.decodeJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	it, err := m.itemInfo(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		a.meError(w, http.StatusNotFound, "that song is not in the engine")
		return
	}
	if err != nil {
		a.meFail(w, r, "read the song", err)
		return
	}
	if it.State != string(musicengine.StateDone) {
		a.meError(w, http.StatusConflict, "the song has to be in the library before you can get its album")
		return
	}
	plan, err := m.Album.Plan(ctx, id)
	switch {
	case errors.Is(err, musicengine.ErrAlbumUnavailable):
		a.meError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		a.deps.Log.Error("musicengine api: planning the full album failed", "item", id, "err", err)
		a.meError(w, http.StatusBadGateway, "could not work out which album that is: "+err.Error())
		return
	}
	rel := plan.Pinned
	if !plan.InStaging && plan.Substitute != nil {
		rel = *plan.Substitute
	}
	year := 0
	if y := meYearRE.FindString(rel.Edition); y != "" {
		year, _ = strconv.Atoi(y)
	}
	release := map[string]any{"artist": rel.Artist, "title": rel.Title, "edition": rel.Edition, "track_count": rel.TrackCount, "year": year}

	if !req.Confirm {
		// The first call only shows which release "the full album" means.
		a.writeJSON(w, http.StatusOK, map[string]any{"release": release, "needs_confirm": true, "decision_id": nil, "state": it.State})
		return
	}
	if !plan.InStaging {
		// Placement needs the release's files; fetching a substitute is not part of the
		// engine yet, so say so rather than start something that cannot finish.
		a.meError(w, http.StatusConflict, "that release is no longer in staging, and fetching it again is not available yet")
		return
	}
	_, existing, ok := a.submitOr503(w, r, jobs.Spec{
		Kind: "musicengine.album", Target: "item:" + strconv.FormatInt(id, 10), Class: "musicengine.album",
		Timeout: 2 * time.Hour,
		Fn: func(ctx context.Context, _ *jobs.Progress) (any, error) {
			res, err := m.Album.Place(ctx, id, musicengine.AlbumOptions{Confirmed: true})
			if err != nil {
				a.deps.Log.Error("musicengine api: placing the full album failed", "item", id, "err", err)
				return nil, err
			}
			return map[string]int{"placed": len(res.Placed), "pending": len(res.Pending), "skipped": len(res.Skipped)}, nil
		}})
	if !ok {
		return
	}
	if existing {
		a.meError(w, http.StatusConflict, "that album is already being placed")
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"release": release, "needs_confirm": false, "decision_id": nil, "state": it.State})
}

// ---- lanes, space, quarantine, alerts ----

var meLanes = []string{musicengine.LaneUsenet, musicengine.LaneTorrent, musicengine.LaneSoulseek, musicengine.LaneYouTube}

func (a *api) meLanes(w http.ResponseWriter, r *http.Request, m *MusicEngineAPI) {
	if !meNeed(a, w, m.DB != nil, "the engine database") {
		return
	}
	type laneRow struct {
		Lane             string `json:"lane"`
		Status           string `json:"status"`
		Reason           string `json:"reason,omitempty"`
		VPNOK            bool   `json:"vpn_ok"`
		CheckedAt        *int64 `json:"checked_at"`
		UnavailableSince *int64 `json:"unavailable_since"`
	}
	have := map[string]laneRow{}
	rows, err := m.DB.QueryContext(r.Context(),
		`SELECT lane, status, reason, vpn_ok, checked_at, unavailable_since FROM mus_lane_status`)
	if err != nil {
		a.meFail(w, r, "read the lane status", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var l laneRow
		var vpn int
		var checked, since int64
		if err := rows.Scan(&l.Lane, &l.Status, &l.Reason, &vpn, &checked, &since); err != nil {
			a.meFail(w, r, "read a lane", err)
			return
		}
		l.VPNOK, l.CheckedAt, l.UnavailableSince = vpn != 0, meNullable(checked), meNullable(since)
		have[l.Lane] = l
	}
	if err := rows.Err(); err != nil {
		a.meFail(w, r, "read the lane status", err)
		return
	}
	out := make([]laneRow, 0, len(meLanes))
	for _, name := range meLanes {
		l, ok := have[name]
		if !ok || (l.Status != "available" && l.Status != "unavailable") {
			// Never checked yet: not usable until a pass has looked at it.
			l = laneRow{Lane: name, Status: "unavailable", Reason: "not checked yet"}
		}
		out = append(out, l)
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"lanes": out})
}

func (a *api) meSpace(w http.ResponseWriter, r *http.Request, m *MusicEngineAPI) {
	if !meNeed(a, w, m.Space != nil, "the space gate") {
		return
	}
	ctx := r.Context()
	st, err := m.Space.Status(ctx)
	if err != nil {
		a.meFail(w, r, "read the disk space", err)
		return
	}
	type vol struct {
		Name       string `json:"name"`
		FreeBytes  int64  `json:"free_bytes"`
		Measurable bool   `json:"measurable"`
	}
	vols := make([]vol, 0, len(st.Volumes))
	for _, v := range st.Volumes {
		name := "downloads"
		if v.Path == m.LibraryPath {
			name = "library"
		}
		vols = append(vols, vol{Name: name, FreeBytes: v.FreeBytes, Measurable: v.Measured})
	}
	quar := map[string]any{"used_bytes": st.QuarantineUsed, "share_bytes": st.QuarantineShare, "full": st.QuarantineShare > 0 && st.QuarantineUsed >= st.QuarantineShare}
	if m.Quarantine != nil {
		u, err := m.Quarantine.Usage(ctx)
		if err != nil {
			a.meFail(w, r, "read the quarantine usage", err)
			return
		}
		quar = map[string]any{"used_bytes": u.UsedBytes, "share_bytes": u.ShareBytes, "full": u.Full}
	}
	var reasons []string
	for _, s := range []string{st.IntakeReason, st.SweepReason} {
		if s != "" {
			reasons = append(reasons, s)
		}
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"volumes": vols, "floor_bytes": st.FloorBytes, "margin_bytes": st.MarginBytes,
		"quarantine": quar, "intake_paused": st.IntakePaused, "sweep_paused": st.SweepPaused,
		"reason": strings.Join(reasons, "; "),
	})
}

// meDays reads an age in days (default 90, the clear action's default).
func (a *api) meDays(w http.ResponseWriter, v *int) (time.Duration, bool) {
	days := int(musicengine.DefaultQuarantineClearAge / (24 * time.Hour))
	if v != nil {
		days = *v
	}
	if days < 0 || days > meMaxClearDays {
		a.meError(w, http.StatusBadRequest, fmt.Sprintf("the age must be between 0 and %d days", meMaxClearDays))
		return 0, false
	}
	return time.Duration(days) * 24 * time.Hour, true
}

func (a *api) meQuarantinePreview(w http.ResponseWriter, r *http.Request, m *MusicEngineAPI) {
	if !meNeed(a, w, m.DB != nil, "the engine database") {
		return
	}
	var days *int
	if s := r.URL.Query().Get("older_than_days"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			a.meError(w, http.StatusBadRequest, "older_than_days must be a whole number")
			return
		}
		days = &n
	}
	age, ok := a.meDays(w, days)
	if !ok {
		return
	}
	// The same rows Quarantine.Clear would take: not cleared, old enough, and not needed
	// by an unsettled journal row.
	var count int
	var bytes int64
	err := m.DB.QueryRowContext(r.Context(),
		`SELECT COUNT(*), COALESCE(SUM(size_bytes), 0) FROM mus_quarantine
		 WHERE cleared_at = 0 AND created_at <= ?
		   AND path NOT IN (SELECT quarantine_path FROM mus_swap_journal WHERE closed_at = 0 AND quarantine_path <> '')`,
		m.now().Add(-age).Unix()).Scan(&count, &bytes)
	if err != nil {
		a.meFail(w, r, "preview the quarantine clear", err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"count": count, "bytes": bytes})
}

func (a *api) meQuarantineClear(w http.ResponseWriter, r *http.Request, m *MusicEngineAPI) {
	if !meNeed(a, w, m.Quarantine != nil, "the quarantine") {
		return
	}
	var req struct {
		OlderThanDays *int `json:"older_than_days"`
	}
	if !a.decodeJSON(w, r, &req) {
		return
	}
	age, ok := a.meDays(w, req.OlderThanDays)
	if !ok {
		return
	}
	if !m.qMu.TryLock() {
		a.meError(w, http.StatusConflict, "a clear is already running")
		return
	}
	defer m.qMu.Unlock()
	rep, err := m.Quarantine.Clear(r.Context(), age)
	if err != nil {
		// Some files may have gone before the error; say how many, and keep the log.
		a.deps.Log.Error("musicengine api: clearing quarantine", "cleared", rep.Cleared, "freed_bytes", rep.FreedBytes, "skipped", rep.Skipped, "err", err)
		a.meError(w, http.StatusInternalServerError,
			fmt.Sprintf("the clear stopped early after removing %d: %v", rep.Cleared, err),
			"removed", rep.Cleared, "freed_bytes", rep.FreedBytes)
		return
	}
	if len(rep.Skipped) > 0 {
		a.deps.Log.Warn("musicengine api: quarantine clear left some versions alone", "skipped", rep.Skipped)
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"removed": rep.Cleared, "freed_bytes": rep.FreedBytes})
}

func (a *api) meAlertFailures(w http.ResponseWriter, r *http.Request, m *MusicEngineAPI) {
	if !meNeed(a, w, m.DB != nil, "the engine database") {
		return
	}
	// Engine alerts that failed for good, or that are still being retried after a failure.
	// The connection's name is shown; its URL never is.
	rows, err := m.DB.QueryContext(r.Context(),
		`SELECT d.event_key, COALESCE(n.name, ''), d.last_error, d.attempts, d.created_at
		 FROM notification_deliveries d LEFT JOIN notifications n ON n.id = d.connection_id
		 WHERE d.event_key LIKE 'musicengine.%'
		   AND (d.status = 'failed' OR (d.status = 'queued' AND d.attempts > 0 AND d.last_error <> ''))
		 ORDER BY d.id DESC LIMIT 100`)
	if err != nil {
		a.meFail(w, r, "read the alert delivery log", err)
		return
	}
	defer rows.Close()
	type failure struct {
		Event      string `json:"event"`
		Connection string `json:"connection"`
		LastError  string `json:"last_error"`
		Attempts   int    `json:"attempts"`
		UpdatedAt  int64  `json:"updated_at"`
	}
	out := []failure{}
	for rows.Next() {
		var f failure
		if err := rows.Scan(&f.Event, &f.Connection, &f.LastError, &f.Attempts, &f.UpdatedAt); err != nil {
			a.meFail(w, r, "read a delivery", err)
			return
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		a.meFail(w, r, "read the alert delivery log", err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"failures": out})
}

// ---- settings and release checks ----

func (a *api) meGetSettings(w http.ResponseWriter, r *http.Request, _ *MusicEngineAPI) {
	a.writeJSON(w, http.StatusOK, a.meSettingsView(r.Context()))
}

// meSettingsView is the stored settings with the auto-swaps flag as it will act: on only
// when both checks are recorded as passed.
func (a *api) meSettingsView(ctx context.Context) MusicEngineSettings {
	s := LoadMusicEngineSettings(ctx, a.deps.Settings)
	s.AutoSwaps = s.AutoSwaps && a.meBothChecksPassed(ctx)
	return s
}

func (a *api) meBothChecksPassed(ctx context.Context) bool {
	return meCheckPassed(ctx, a.deps.Settings, MusicEngineKeyCalibration, a.deps.Log.Warn) &&
		meCheckPassed(ctx, a.deps.Settings, MusicEngineKeyPathTest, a.deps.Log.Warn)
}

func meGBOK(v float64, min float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= min && v <= 1e7
}

func (a *api) mePutSettings(w http.ResponseWriter, r *http.Request, m *MusicEngineAPI) {
	var req struct {
		WatchedPlaylist   *string  `json:"watched_playlist"`
		FloorGB           *float64 `json:"floor_gb"`
		MarginGB          *float64 `json:"margin_gb"`
		QuarantineShareGB *float64 `json:"quarantine_share_gb"`
		AutoSwaps         *bool    `json:"auto_swaps"`
		Enabled           *bool    `json:"enabled"`
	}
	if !a.decodeJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	// One writer at a time, and the release checks are read inside the lock so a check
	// recorded a moment ago counts and one being recorded cannot slip past.
	m.mu.Lock()
	defer m.mu.Unlock()

	cur := LoadMusicEngineSettings(ctx, a.deps.Settings)
	if req.WatchedPlaylist != nil {
		id, err := spotify.ParsePlaylistID(*req.WatchedPlaylist)
		if err != nil {
			a.meError(w, http.StatusBadRequest, "that is not a Spotify playlist: "+err.Error())
			return
		}
		cur.WatchedPlaylist = id
	}
	if req.FloorGB != nil {
		if !meGBOK(*req.FloorGB, 0) {
			a.meError(w, http.StatusBadRequest, "the free-space floor must be 0 GB or more")
			return
		}
		cur.FloorGB = *req.FloorGB
	}
	if req.MarginGB != nil {
		if !meGBOK(*req.MarginGB, 0) {
			a.meError(w, http.StatusBadRequest, "the resume margin must be 0 GB or more")
			return
		}
		cur.MarginGB = *req.MarginGB
	}
	if req.QuarantineShareGB != nil {
		if !meGBOK(*req.QuarantineShareGB, 1) {
			a.meError(w, http.StatusBadRequest, "the quarantine share must be at least 1 GB")
			return
		}
		cur.QuarantineShareGB = *req.QuarantineShareGB
	}
	if req.Enabled != nil {
		cur.Enabled = *req.Enabled
	}
	if req.AutoSwaps != nil {
		cur.AutoSwaps = *req.AutoSwaps
	}
	// KTD16: checked on the server, on every save that leaves the setting on.
	if cur.AutoSwaps && !a.meBothChecksPassed(ctx) {
		a.meError(w, http.StatusConflict, meAutoSwapsRefusal)
		return
	}

	writes := []struct{ key, val string }{
		{MusicEngineKeyPlaylist, cur.WatchedPlaylist},
		{MusicEngineKeyFloorGB, strconv.FormatFloat(cur.FloorGB, 'f', -1, 64)},
		{MusicEngineKeyMarginGB, strconv.FormatFloat(cur.MarginGB, 'f', -1, 64)},
		{MusicEngineKeyQuarGB, strconv.FormatFloat(cur.QuarantineShareGB, 'f', -1, 64)},
		{MusicEngineKeyAutoSwaps, strconv.FormatBool(cur.AutoSwaps)},
		{MusicEngineKeyEnabled, strconv.FormatBool(cur.Enabled)},
	}
	for _, wr := range writes {
		if err := a.deps.Settings.Set(ctx, wr.key, wr.val); err != nil {
			a.meFail(w, r, "save the settings", err)
			return
		}
	}
	a.writeJSON(w, http.StatusOK, a.meSettingsView(ctx))
}

func (a *api) meChecksView(ctx context.Context) map[string]meCheck {
	return map[string]meCheck{
		"calibration": meLoadCheck(ctx, a.deps.Settings, MusicEngineKeyCalibration, a.deps.Log.Warn),
		"path_test":   meLoadCheck(ctx, a.deps.Settings, MusicEngineKeyPathTest, a.deps.Log.Warn),
	}
}

func (a *api) meGetReleaseChecks(w http.ResponseWriter, r *http.Request, _ *MusicEngineAPI) {
	a.writeJSON(w, http.StatusOK, a.meChecksView(r.Context()))
}

// meRecordCheck stores one check's result. A check recorded as failed also switches the
// automatic-swaps setting off: the checks are the condition for it being on.
func (a *api) meRecordCheck(ctx context.Context, m *MusicEngineAPI, key string, passed bool, summary string) error {
	at := m.now().Unix()
	raw, err := json.Marshal(meCheck{Passed: &passed, At: &at, Summary: summary})
	if err != nil {
		return fmt.Errorf("encoding the release check: %w", err)
	}
	if err := a.deps.Settings.Set(ctx, key, string(raw)); err != nil {
		return err
	}
	if !passed && a.deps.Settings.GetBool(ctx, MusicEngineKeyAutoSwaps, false) {
		if err := a.deps.Settings.SetBool(ctx, MusicEngineKeyAutoSwaps, false); err != nil {
			return err
		}
		a.deps.Log.Warn("musicengine api: a release check failed, so automatic swaps were switched off", "check", key)
	}
	return nil
}

func (a *api) mePathTest(w http.ResponseWriter, r *http.Request, m *MusicEngineAPI) {
	var req struct {
		Passed  bool   `json:"passed"`
		Summary string `json:"summary"`
	}
	if !a.decodeJSON(w, r, &req) {
		return
	}
	req.Summary = strings.TrimSpace(req.Summary)
	if req.Passed && req.Summary == "" {
		a.meError(w, http.StatusBadRequest, "say what was read before and after the test swap")
		return
	}
	if len(req.Summary) > 4000 {
		a.meError(w, http.StatusBadRequest, "the summary is too long")
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := a.meRecordCheck(r.Context(), m, MusicEngineKeyPathTest, req.Passed, req.Summary); err != nil {
		a.meFail(w, r, "record the path test", err)
		return
	}
	a.writeJSON(w, http.StatusOK, a.meChecksView(r.Context()))
}

type meCalRow struct {
	Path     string `json:"path"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
	OK       bool   `json:"ok"`
}

// meUnderRoot returns path cleaned, or an error when it is not an absolute path inside root
// (by name or through a symlink).
func meUnderRoot(root, path string) (string, error) {
	if root == "" || !filepath.IsAbs(root) {
		return "", errors.New("the music library folder is not configured")
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("must be a full path")
	}
	root, path = filepath.Clean(root), filepath.Clean(path)
	inside := func(r, p string) bool {
		rel, err := filepath.Rel(r, p)
		return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	if !inside(root, path) {
		return "", errors.New("is outside the music library")
	}
	// A symlink inside the library that points out of it does not count as inside.
	if real, err := filepath.EvalSymlinks(path); err == nil {
		realRoot := root
		if rr, err := filepath.EvalSymlinks(root); err == nil {
			realRoot = rr
		}
		if !inside(realRoot, real) {
			return "", errors.New("points outside the music library")
		}
	}
	return path, nil
}

func (a *api) meCalibration(w http.ResponseWriter, r *http.Request, m *MusicEngineAPI) {
	if !meNeed(a, w, m.Checker != nil && m.LibraryRoot != nil, "the audio checker") {
		return
	}
	var req struct {
		Rows []struct {
			Path     string `json:"path"`
			Expected string `json:"expected"`
		} `json:"rows"`
	}
	if !a.decodeJSON(w, r, &req) {
		return
	}
	root := m.LibraryRoot()
	type job struct{ path, expected string }
	var todo []job
	var genuine, bad int
	for i, row := range req.Rows {
		p := strings.TrimSpace(row.Path)
		// Blank and "#" rows are comments from a pasted calibration list.
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		exp := audiocheck.Verdict(strings.ToUpper(strings.TrimSpace(row.Expected)))
		switch {
		case exp == audiocheck.Genuine:
			genuine++
		case exp.Bad():
			bad++
		default:
			a.meError(w, http.StatusBadRequest, fmt.Sprintf("row %d: %q is not a verdict to expect (use GENUINE, SUSPECT, FAKE, UPSAMPLED, FAKE_BIT_DEPTH or CORRUPT)", i+1, row.Expected))
			return
		}
		clean, err := meUnderRoot(root, p)
		if err != nil {
			a.meError(w, http.StatusBadRequest, fmt.Sprintf("row %d: %s %s", i+1, p, err.Error()))
			return
		}
		todo = append(todo, job{clean, string(exp)})
	}
	if len(todo) > meMaxCalibrationRows {
		a.meError(w, http.StatusBadRequest, fmt.Sprintf("at most %d files per run", meMaxCalibrationRows))
		return
	}
	// A run of only genuine files, or only fakes, cannot show the checker tells them apart.
	if genuine == 0 || bad == 0 {
		a.meError(w, http.StatusBadRequest, "the calibration needs at least one known-genuine file and one known-fake file")
		return
	}
	if !m.calMu.TryLock() {
		a.meError(w, http.StatusConflict, "a calibration run is already going")
		return
	}
	defer m.calMu.Unlock()

	ctx := r.Context()
	rows := make([]meCalRow, 0, len(todo))
	pass := true
	var wrong []string
	for _, j := range todo {
		res := m.Checker.Check(ctx, j.path)
		row := meCalRow{Path: j.path, Expected: j.expected, Actual: string(res.Verdict), OK: string(res.Verdict) == j.expected}
		if !row.OK {
			pass = false
			wrong = append(wrong, fmt.Sprintf("%s: expected %s, got %s (%s)", filepath.Base(j.path), row.Expected, row.Actual, res.Reason))
		}
		rows = append(rows, row)
	}
	if err := ctx.Err(); err != nil {
		// The caller went away mid-run; the verdicts are Blind, not evidence. Record nothing.
		a.deps.Log.Warn("musicengine api: calibration interrupted, nothing recorded", "err", err)
		return
	}
	summary := fmt.Sprintf("%d of %d files classified as expected", len(rows)-len(wrong), len(rows))
	if len(wrong) > 0 {
		summary += "; " + strings.Join(wrong, "; ")
	}
	m.mu.Lock()
	err := a.meRecordCheck(ctx, m, MusicEngineKeyCalibration, pass, summary)
	m.mu.Unlock()
	if err != nil {
		a.meFail(w, r, "record the calibration result", err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"passed": pass, "summary": summary, "rows": rows})
}

// ---- heartbeat ----

func (a *api) meHeartbeat(w http.ResponseWriter, _ *http.Request, m *MusicEngineAPI) {
	if !meNeed(a, w, m.Heartbeat != nil, "the heartbeat") {
		return
	}
	// The one thing this route says. U12's status calls the field last_tick_at; the
	// watchdog reads last_tick.
	a.writeJSON(w, http.StatusOK, map[string]int64{"last_tick": m.Heartbeat.Status().LastTickAt})
}
