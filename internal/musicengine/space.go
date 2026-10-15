package musicengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tristenlammi/arrmada/internal/diskspace"
	"github.com/tristenlammi/arrmada/internal/safego"
)

// The space gate (U12, KTD10, R27, R28, R34). It implements SpaceGate and, through
// SpaceReserver, the reservations U9 takes before a download starts.
//
// Free space is measured per volume (the library's and the downloads'), so one disk
// filling up cannot hide behind another's room. A volume that cannot be measured, or does
// not answer in time, counts as full: both kinds of work pause and one alert is raised.
// The gate never deletes anything; it only says no.
//
// Pausing is ordered (R28). The upgrade sweep pauses first, when free space drops under
// floor + margin; new music pauses at the floor, and a reservation that would cross the
// floor pauses it too. Resuming is the reverse and has hysteresis: new music resumes once
// free space is back to floor + margin, the sweep once it is back to floor + 2*margin.
// Quarantine has its own share (R34); when it is used up the sweep pauses and intake runs
// on.

// Defaults for SpaceSettings (R28, R34).
const (
	DefaultSpaceFloor      int64 = 100 << 30
	DefaultSpaceMargin     int64 = 25 << 30
	DefaultQuarantineShare int64 = 200 << 30
)

const spaceMeasureTimeout = 10 * time.Second

// SpaceSettings are the byte amounts the gate reads fresh on every call. Zero means the
// default.
type SpaceSettings struct {
	Floor           int64 // free bytes to keep on each volume
	Margin          int64 // extra free bytes needed before work resumes
	QuarantineShare int64 // bytes of replaced versions kept before the sweep pauses
}

func (s SpaceSettings) withDefaults() SpaceSettings {
	if s.Floor <= 0 {
		s.Floor = DefaultSpaceFloor
	}
	if s.Margin <= 0 {
		s.Margin = DefaultSpaceMargin
	}
	if s.QuarantineShare <= 0 {
		s.QuarantineShare = DefaultQuarantineShare
	}
	return s
}

// SpaceReserver is how U9 holds room for a download before it starts (KTD10). A
// reservation counts against every volume until Release; reserving again for the same item
// replaces its earlier amount.
type SpaceReserver interface {
	Reserve(ctx context.Context, itemID, bytes int64) (ok bool, reason string)
	Release(ctx context.Context, itemID int64)
}

// SpaceConfig wires a Space. Library and downloads paths are the two volumes the engine
// writes to; both are required (an empty path is an unmeasurable volume).
type SpaceConfig struct {
	DB            *sql.DB
	LibraryPath   string
	DownloadsPath string
	Settings      func() SpaceSettings
	// Measure is diskspace.Of unless a test injects another.
	Measure        func(path string) (diskspace.Usage, bool)
	MeasureTimeout time.Duration
	Notifier       Notifier
	Now            func() time.Time
	Log            *slog.Logger
}

// Space is the space gate. Safe for concurrent use.
type Space struct {
	db       *sql.DB
	paths    []string
	settings func() SpaceSettings
	measure  func(string) (diskspace.Usage, bool)
	timeout  time.Duration
	notifier Notifier
	now      func() time.Time
	log      *slog.Logger

	mu           sync.Mutex
	loaded       bool
	reserved     map[int64]int64
	sweepPaused  bool
	intakePaused bool
	episode      int64 // unix time the current pause episode began, 0 when none
	quarFull     bool
	quarEpisode  int64
	why          spaceWhy
	alerted      map[string]bool
	// incidentCleared notes incidents already closed in the database, so recovery writes once.
	incidentCleared map[string]bool
}

type spaceWhy struct{ sweep, intake, quarantine string }

var (
	_ SpaceGate     = (*Space)(nil)
	_ SpaceReserver = (*Space)(nil)
)

// NewSpace builds the gate.
func NewSpace(cfg SpaceConfig) (*Space, error) {
	if cfg.DB == nil {
		return nil, errors.New("musicengine: space gate needs a database")
	}
	s := &Space{
		db: cfg.DB, settings: cfg.Settings, measure: cfg.Measure, timeout: cfg.MeasureTimeout,
		notifier: cfg.Notifier, now: cfg.Now, log: cfg.Log,
		reserved: map[int64]int64{}, alerted: map[string]bool{},
	}
	for _, p := range []string{cfg.LibraryPath, cfg.DownloadsPath} {
		dup := false
		for _, q := range s.paths {
			dup = dup || q == p
		}
		if !dup {
			s.paths = append(s.paths, p)
		}
	}
	if s.settings == nil {
		s.settings = func() SpaceSettings { return SpaceSettings{} }
	}
	if s.measure == nil {
		s.measure = diskspace.Of
	}
	if s.timeout <= 0 {
		s.timeout = spaceMeasureTimeout
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.notifier == nil {
		s.notifier = logNotifier{log: s.log}
	}
	return s, nil
}

// spaceVolume is one measurement.
type spaceVolume struct {
	Path     string
	Free     int64
	Total    int64
	Measured bool
}

// spaceMeasure measures a path with a hard deadline: a dead mount can hang statfs, and a
// volume that does not answer is a volume that cannot be measured.
func (s *Space) spaceMeasure(ctx context.Context, path string) spaceVolume {
	v := spaceVolume{Path: path}
	if path == "" {
		return v
	}
	type result struct {
		u  diskspace.Usage
		ok bool
	}
	ch := make(chan result, 1)
	safego.Go(s.log, "musicengine: measure space", func() {
		u, ok := s.measure(path)
		ch <- result{u, ok}
	})
	timer := time.NewTimer(s.timeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		if r.ok {
			v.Free, v.Total, v.Measured = int64(r.u.FreeBytes), int64(r.u.TotalBytes), true
		}
	case <-timer.C:
		s.log.Warn("musicengine: space measurement timed out", "path", path, "after", s.timeout)
	case <-ctx.Done():
	}
	return v
}

func (s *Space) spaceLoad(ctx context.Context) error {
	if s.loaded {
		return nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT item_id, SUM(reserved_bytes) FROM mus_candidates
		 WHERE state = 'downloading' AND reserved_bytes > 0 GROUP BY item_id`)
	if err != nil {
		return fmt.Errorf("reading space reservations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, n int64
		if err := rows.Scan(&id, &n); err != nil {
			return fmt.Errorf("scanning a space reservation: %w", err)
		}
		if _, held := s.reserved[id]; !held {
			s.reserved[id] = n
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading space reservations: %w", err)
	}
	s.loaded = true
	return nil
}

func (s *Space) reservedTotalLocked() int64 {
	var t int64
	for _, n := range s.reserved {
		t += n
	}
	return t
}

// spaceQuarantineUsed is the bytes of quarantined versions Blair has not cleared.
func (s *Space) spaceQuarantineUsed(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes), 0) FROM mus_quarantine WHERE cleared_at = 0`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("reading quarantine usage: %w", err)
	}
	return n, nil
}

type spaceAlert struct {
	key string
	a   Alert
}

// spaceReading is one pass over the volumes.
type spaceReading struct {
	vols    []spaceVolume
	minFree int64 // lowest free space across volumes, less the held reservations
	bad     string
}

func (s *Space) readLocked(ctx context.Context) spaceReading {
	r := spaceReading{minFree: -1}
	held := s.reservedTotalLocked()
	for _, p := range s.paths {
		v := s.spaceMeasure(ctx, p)
		r.vols = append(r.vols, v)
		if !v.Measured {
			if r.bad == "" {
				r.bad = p
			}
			continue
		}
		eff := v.Free - held
		if eff < 0 {
			eff = 0
		}
		if r.minFree < 0 || eff < r.minFree {
			r.minFree = eff
		}
	}
	return r
}

// updateLocked moves the pause flags for a reading and returns the alerts that are due.
func (s *Space) updateLocked(ctx context.Context, r spaceReading, st SpaceSettings) []spaceAlert {
	now := s.now()
	wasPaused := s.sweepPaused || s.intakePaused
	storage := ""
	switch {
	case r.bad != "" || len(s.paths) == 0:
		storage = r.bad
		if storage == "" {
			storage = "(no volume configured)"
		}
		s.sweepPaused, s.intakePaused = true, true
		why := "Can't measure free space on " + storage
		s.why.sweep, s.why.intake = why, why
	default:
		if s.intakePaused {
			if r.minFree >= st.Floor+st.Margin {
				s.intakePaused = false
			}
		} else if r.minFree < st.Floor {
			s.intakePaused = true
			s.why.intake = fmt.Sprintf("Free space (%s) is below the %s minimum", spaceGB(r.minFree), spaceGB(st.Floor))
		}
		if s.sweepPaused {
			if r.minFree >= st.Floor+2*st.Margin {
				s.sweepPaused = false
			}
		} else if r.minFree < st.Floor+st.Margin {
			s.sweepPaused = true
			s.why.sweep = fmt.Sprintf("Free space (%s) is under the %s minimum plus a %s safety margin", spaceGB(r.minFree), spaceGB(st.Floor), spaceGB(st.Margin))
		}
	}
	if !s.sweepPaused && !s.intakePaused {
		s.episode = 0
		s.incidentClear(ctx, incidentSpacePause, now.Unix())
	} else if !wasPaused || s.episode == 0 {
		s.episode = s.incidentStart(ctx, incidentSpacePause, now.Unix())
	}

	var out []spaceAlert
	if s.episode != 0 {
		switch {
		case storage != "":
			key := fmt.Sprintf("mus-space:%d:storage", s.episode)
			out = append(out, spaceAlert{key, Alert{
				Kind: AlertPausedForSpace, Key: key,
				Subject: "Music engine paused: storage cannot be read",
				Body:    fmt.Sprintf("The engine could not measure free space on %s, so the upgrade sweep and new music are both paused. Nothing was deleted. It resumes by itself when the volume answers and has room.", storage),
			}})
		case s.intakePaused:
			key := fmt.Sprintf("mus-space:%d:intake", s.episode)
			out = append(out, spaceAlert{key, Alert{
				Kind: AlertPausedForSpace, Key: key,
				Subject: "Music engine paused: new music is waiting for space",
				Body:    "The upgrade sweep and new music are both paused. " + s.why.intake + ". Nothing was deleted. New music resumes at " + spaceGB(st.Floor+st.Margin) + " free.",
			}})
		case s.sweepPaused:
			key := fmt.Sprintf("mus-space:%d:sweep", s.episode)
			out = append(out, spaceAlert{key, Alert{
				Kind: AlertPausedForSpace, Key: key,
				Subject: "Music engine: upgrade sweep paused for space",
				Body:    "The upgrade sweep is paused: " + s.why.sweep + ". New music still runs. Nothing was deleted.",
			}})
		}
	}
	return filterAlerted(out, s.alerted)
}

func filterAlerted(in []spaceAlert, done map[string]bool) []spaceAlert {
	var out []spaceAlert
	for _, a := range in {
		if !done[a.key] {
			out = append(out, a)
		}
	}
	return out
}

// sendAlerts queues alerts outside the lock and records the ones the notifier accepted, so
// a refusal is retried on the next call and an accepted one is not repeated.
func (s *Space) sendAlerts(ctx context.Context, alerts []spaceAlert) {
	for _, a := range alerts {
		if err := s.notifier.Alert(ctx, a.a); err != nil {
			s.log.Warn("musicengine: space alert not queued, will retry", "key", a.key, "err", err)
			continue
		}
		s.mu.Lock()
		s.alerted[a.key] = true
		s.mu.Unlock()
	}
}

// evaluate takes one reading, updates the flags and returns the quarantine verdict.
func (s *Space) evaluate(ctx context.Context) (r spaceReading, st SpaceSettings, quarOK bool, alerts []spaceAlert, err error) {
	st = s.settings().withDefaults()
	if err = s.spaceLoad(ctx); err != nil {
		return r, st, false, nil, err
	}
	r = s.readLocked(ctx)
	alerts = s.updateLocked(ctx, r, st)

	used, qerr := s.spaceQuarantineUsed(ctx)
	switch {
	case qerr != nil:
		s.log.Warn("musicengine: quarantine usage unreadable, sweep paused", "err", qerr)
		s.why.quarantine = "Can't read how much space replaced files use: " + qerr.Error()
		quarOK = false
	case used >= st.QuarantineShare:
		s.why.quarantine = fmt.Sprintf("Replaced files kept aside use %s, which is their full %s allowance", spaceGB(used), spaceGB(st.QuarantineShare))
		quarOK = false
		if !s.quarFull {
			s.quarFull = true
			s.quarEpisode = s.incidentStart(ctx, incidentQuarantineFull, s.now().Unix())
		}
		key := fmt.Sprintf("mus-quarantine-full:%d", s.quarEpisode)
		alerts = append(alerts, filterAlerted([]spaceAlert{{key, Alert{
			Kind: AlertQuarantineFull, Key: key,
			Subject: "Space for replaced music files is full",
			Body:    "Replaced versions use " + spaceGB(used) + " of their " + spaceGB(st.QuarantineShare) + " allowance, so the upgrade sweep is paused. New music still runs. Clear old versions on the music engine page to resume the sweep.",
		}}}, s.alerted)...)
	default:
		s.quarFull, s.quarEpisode = false, 0
		s.incidentClear(ctx, incidentQuarantineFull, s.now().Unix())
		quarOK = true
	}
	return r, st, quarOK, alerts, nil
}

// Allow implements SpaceGate. A gate that cannot decide says no.
func (s *Space) Allow(ctx context.Context, kind WorkKind) (bool, string) {
	s.mu.Lock()
	_, _, quarOK, alerts, err := s.evaluate(ctx)
	var ok bool
	var reason string
	switch {
	case err != nil:
		ok, reason = false, err.Error()
	case kind == WorkSweep && s.sweepPaused:
		reason = s.why.sweep
	case kind == WorkSweep && !quarOK:
		reason = s.why.quarantine
	case kind == WorkIntake && s.intakePaused:
		reason = s.why.intake
	default:
		ok = true
	}
	s.mu.Unlock()
	s.sendAlerts(ctx, alerts)
	return ok, reason
}

// Reserve implements SpaceReserver.
func (s *Space) Reserve(ctx context.Context, itemID, bytes int64) (bool, string) {
	if bytes <= 0 {
		return false, "reservation size must be positive"
	}
	kind := WorkIntake
	var k string
	if err := s.db.QueryRowContext(ctx, `SELECT kind FROM mus_items WHERE id = ?`, itemID).Scan(&k); err == nil && k == KindSweep {
		kind = WorkSweep
	}

	s.mu.Lock()
	prev, had := s.reserved[itemID]
	if err := s.spaceLoad(ctx); err != nil {
		s.mu.Unlock()
		return false, err.Error()
	}
	delete(s.reserved, itemID) // an item's new amount replaces its old one
	r, st, quarOK, alerts, err := s.evaluate(ctx)
	ok, reason := false, ""
	switch {
	case err != nil:
		reason = err.Error()
	case kind == WorkSweep && s.sweepPaused:
		reason = s.why.sweep
	case kind == WorkSweep && !quarOK:
		reason = s.why.quarantine
	case kind == WorkIntake && s.intakePaused:
		reason = s.why.intake
	case r.minFree-bytes < st.Floor:
		// The download would take free space under the floor: pause new music too, so the
		// next item does not try the same thing (AE14).
		reason = fmt.Sprintf("A %s download would leave only %s free, under the %s minimum", spaceGB(bytes), spaceGB(r.minFree-bytes), spaceGB(st.Floor))
		s.intakePaused, s.sweepPaused = true, true
		s.why.intake, s.why.sweep = reason, reason
		if s.episode == 0 {
			s.episode = s.incidentStart(ctx, incidentSpacePause, s.now().Unix())
		}
		key := fmt.Sprintf("mus-space:%d:intake", s.episode)
		alerts = append(alerts, filterAlerted([]spaceAlert{{key, Alert{
			Kind: AlertPausedForSpace, Key: key,
			Subject: "Music engine paused: new music is waiting for space",
			Body:    "The upgrade sweep and new music are both paused. " + reason + ". Nothing was deleted. New music resumes at " + spaceGB(st.Floor+st.Margin) + " free.",
		}}}, s.alerted)...)
	default:
		ok = true
	}
	if ok {
		s.reserved[itemID] = bytes
	} else if had {
		s.reserved[itemID] = prev
	}
	s.mu.Unlock()
	s.sendAlerts(ctx, alerts)
	return ok, reason
}

// Release implements SpaceReserver. Releasing an item with no reservation does nothing.
func (s *Space) Release(_ context.Context, itemID int64) {
	s.mu.Lock()
	delete(s.reserved, itemID)
	s.mu.Unlock()
}

// SpaceStatus is what the status page shows (U13): per-volume room, what is held, and why
// each kind of work is paused.
type SpaceStatus struct {
	Volumes         []SpaceVolumeStatus `json:"volumes"`
	ReservedBytes   int64               `json:"reserved_bytes"`
	FloorBytes      int64               `json:"floor_bytes"`
	MarginBytes     int64               `json:"margin_bytes"`
	QuarantineUsed  int64               `json:"quarantine_used_bytes"`
	QuarantineShare int64               `json:"quarantine_share_bytes"`
	IntakePaused    bool                `json:"intake_paused"`
	IntakeReason    string              `json:"intake_reason,omitempty"`
	SweepPaused     bool                `json:"sweep_paused"`
	SweepReason     string              `json:"sweep_reason,omitempty"`
}

// SpaceVolumeStatus is one measured volume.
type SpaceVolumeStatus struct {
	Path       string `json:"path"`
	Measured   bool   `json:"measured"`
	FreeBytes  int64  `json:"free_bytes"`
	TotalBytes int64  `json:"total_bytes"`
}

// Status takes a fresh reading and reports it.
func (s *Space) Status(ctx context.Context) (SpaceStatus, error) {
	s.mu.Lock()
	r, st, quarOK, alerts, err := s.evaluate(ctx)
	out := SpaceStatus{
		ReservedBytes: s.reservedTotalLocked(), FloorBytes: st.Floor, MarginBytes: st.Margin,
		QuarantineShare: st.QuarantineShare,
		IntakePaused:    s.intakePaused, SweepPaused: s.sweepPaused || !quarOK,
	}
	if s.intakePaused {
		out.IntakeReason = s.why.intake
	}
	switch {
	case s.sweepPaused:
		out.SweepReason = s.why.sweep
	case !quarOK:
		out.SweepReason = s.why.quarantine
	}
	for _, v := range r.vols {
		out.Volumes = append(out.Volumes, SpaceVolumeStatus{Path: v.Path, Measured: v.Measured, FreeBytes: v.Free, TotalBytes: v.Total})
	}
	s.mu.Unlock()
	s.sendAlerts(ctx, alerts)
	if err != nil {
		return out, err
	}
	used, qerr := s.spaceQuarantineUsed(ctx)
	out.QuarantineUsed = used
	return out, qerr
}

func spaceGB(b int64) string {
	return fmt.Sprintf("%.1f GB", float64(b)/float64(1<<30))
}
