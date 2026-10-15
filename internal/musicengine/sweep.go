package musicengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/tristenlammi/arrmada/internal/store"
)

// The upgrade sweep (R20, R23, R26). Run is the scheduler task: each call settles what earlier
// visits left behind and, when there is room, opens one new sweep item for the next track in
// this order:
//
//  1. tracks in any non-smart Plex playlist,
//  2. tracks rated 4 stars or higher,
//  3. tracks with a Plex play,
//  4. the rest.
//
// A track rated 1 star is skipped even when a playlist holds it. The sweep is deliberately
// slow: at most MaxActive items are in flight (default 1), and a track that found nothing
// better is revisited on a doubling interval, 7 days first, capped at 90 days.
//
// A sweep item starts in `searching`: the library file is already known, and the swap step
// rechecks it and compares before anything is replaced, so the library-check state would only
// decide "already have it" for a genuine file, which is exactly the file a sweep looks to improve.

// Defaults for the R26 retry interval.
const (
	DefaultSweepBase = 7 * 24 * time.Hour
	DefaultSweepMax  = 90 * 24 * time.Hour
	// sweepDeferAfter is when a deferred track (kept playing) is visited again.
	sweepDeferAfter = 24 * time.Hour
	// sweepCandidatesPerRun bounds how many tracks one run looks at before it gives up.
	sweepCandidatesPerRun = 25
	// Plex stores ratings on a 0-10 scale: each star is 2.
	sweepOneStarMax     = 2.0
	sweepFourStarMin    = 8.0
	sweepRecheckMissing = 7 * 24 * time.Hour
)

// Sweep schedules upgrade visits. Safe to call from the scheduler; Run never runs twice at once
// in effect, because it will not open an item while MaxActive are in flight.
type Sweep struct {
	cfg SweepConfig
	log *slog.Logger
}

// SweepTrack is a library track the sweep may visit.
type SweepTrack struct {
	ID          int64
	RecordingID int64
	PlexPath    string
	Artist      string
	Title       string
	Album       string
	Rating      float64
	PlayCount   int
	Playlists   int
}

// SweepConfig wires the sweep.
type SweepConfig struct {
	DB *sql.DB
	// Space is the gate; the sweep opens nothing while it says no (R28). Nil allows.
	Space SpaceGate
	// Swaps, when set, has its leftover undo rows resumed on each run.
	Swaps *Swaps
	// Local maps a library path as Plex sees it to this process's path, to confirm the file
	// exists. Nil uses the Swaps' placement mapping, else the path as is.
	Local func(plexPath string) string
	// Resolve gives a track with no recording id one (from its tags and MusicBrainz). Nil
	// means those tracks are skipped: the sweep needs a recording to hold the one-active-item
	// rule on.
	Resolve func(ctx context.Context, t SweepTrack) (recordingID int64, err error)
	// MaxActive bounds the sweep items in flight; 0 means 1.
	MaxActive int
	// Base and Max are the R26 interval bounds; 0 means the defaults.
	Base, Max time.Duration
	Now       func() time.Time
	Log       *slog.Logger
}

// NewSweep validates the config.
func NewSweep(cfg SweepConfig) (*Sweep, error) {
	if cfg.DB == nil {
		return nil, errors.New("musicengine sweep: no database")
	}
	if cfg.MaxActive <= 0 {
		cfg.MaxActive = 1
	}
	if cfg.Base <= 0 {
		cfg.Base = DefaultSweepBase
	}
	if cfg.Max <= 0 {
		cfg.Max = DefaultSweepMax
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Sweep{cfg: cfg, log: log}, nil
}

// SweepReport says what one run did.
type SweepReport struct {
	// Paused is set, with Reason, when the space gate said no.
	Paused bool
	Reason string
	// Active is the number of sweep items in flight when the run started.
	Active int
	// Created is the new item's id, 0 when none was opened; Track is its library track.
	Created int64
	Track   int64
	// Skipped counts tracks looked at and passed over (file missing, recording unresolved).
	Skipped int
	// Settled counts leftovers closed (sweep misses, kept candidates blocked).
	SettledMisses, BlockedKept int
}

// Run is the scheduler task.
func (s *Sweep) Run(ctx context.Context) (SweepReport, error) {
	var rep SweepReport
	var errs []error

	if s.cfg.Swaps != nil {
		if err := s.cfg.Swaps.Resume(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	n, err := s.settleMisses(ctx)
	rep.SettledMisses = n
	if err != nil {
		errs = append(errs, err)
	}
	if n, err := s.settleKeeps(ctx); err != nil {
		errs = append(errs, err)
	} else {
		rep.BlockedKept = n
	}

	if s.cfg.Space != nil {
		if ok, why := s.cfg.Space.Allow(ctx, WorkSweep); !ok {
			rep.Paused, rep.Reason = true, why
			return rep, errors.Join(errs...)
		}
	}
	active, err := s.activeCount(ctx)
	if err != nil {
		return rep, errors.Join(append(errs, err)...)
	}
	rep.Active = active
	if active >= s.cfg.MaxActive {
		return rep, errors.Join(errs...)
	}
	id, track, skipped, err := s.openNext(ctx)
	rep.Created, rep.Track, rep.Skipped = id, track, skipped
	if err != nil {
		errs = append(errs, err)
	}
	return rep, errors.Join(errs...)
}

// activeCount is the sweep items doing work. An item waiting for Blair or paused holds its
// recording but does not hold up the next track.
func (s *Sweep) activeCount(ctx context.Context) (int, error) {
	var n int
	err := s.cfg.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mus_items WHERE kind = ? AND state NOT IN ('done','cancelled','dropped','paused','waiting_blair')`,
		KindSweep).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting active sweep items: %w", err)
	}
	return n, nil
}

// NextTracks lists the tracks the sweep would visit next, in order, without opening anything.
func (s *Sweep) NextTracks(ctx context.Context, limit int) ([]SweepTrack, error) {
	return sweepEligible(ctx, s.cfg.DB, s.cfg.Now(), s.cfg.Resolve != nil, limit)
}

// sweepEligible is the R20 order. The eligibility rules are in SQL so the order and the skips
// can be read in one place:
//   - a track rated 1 star or less is out, playlist or not;
//   - a track with an unfinished item (by track or by recording) is out (R23, KTD3);
//   - a track not due yet (R26) is out.
func sweepEligible(ctx context.Context, q store.Execer, now time.Time, withUnresolved bool, limit int) ([]SweepTrack, error) {
	where := "AND t.recording_id IS NOT NULL"
	if withUnresolved {
		where = ""
	}
	rows, err := q.QueryContext(ctx,
		`SELECT t.id, COALESCE(t.recording_id, 0), t.path, t.artist, t.title, t.album,
		        COALESCE(t.user_rating, 0), t.play_count, t.playlist_count
		 FROM mus_library_tracks t
		 WHERE t.path <> '' AND t.plex_rating_key <> '' AND t.next_sweep_at <= ?
		   AND (t.user_rating IS NULL OR t.user_rating <= 0 OR t.user_rating > ?)
		   AND NOT EXISTS (SELECT 1 FROM mus_items i WHERE i.track_id = t.id AND i.state NOT IN ('done','cancelled','dropped'))
		   AND (t.recording_id IS NULL OR NOT EXISTS
		        (SELECT 1 FROM mus_items i WHERE i.recording_id = t.recording_id AND i.state NOT IN ('done','cancelled','dropped')))
		   `+where+`
		 ORDER BY CASE WHEN t.playlist_count > 0 THEN 0
		               WHEN t.user_rating >= ? THEN 1
		               WHEN t.play_count > 0 THEN 2
		               ELSE 3 END,
		          t.next_sweep_at, t.id
		 LIMIT ?`, now.Unix(), sweepOneStarMax, sweepFourStarMin, limit)
	if err != nil {
		return nil, fmt.Errorf("listing tracks for the sweep: %w", err)
	}
	defer rows.Close()
	var out []SweepTrack
	for rows.Next() {
		var t SweepTrack
		if err := rows.Scan(&t.ID, &t.RecordingID, &t.PlexPath, &t.Artist, &t.Title, &t.Album, &t.Rating, &t.PlayCount, &t.Playlists); err != nil {
			return nil, fmt.Errorf("scanning sweep track: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Sweep) localPath(plexPath string) string {
	switch {
	case s.cfg.Local != nil:
		return s.cfg.Local(plexPath)
	case s.cfg.Swaps != nil && s.cfg.Swaps.p != nil:
		return s.cfg.Swaps.p.paths.toLocal(plexPath)
	}
	return plexPath
}

// openNext opens one sweep item for the first track that can be visited.
func (s *Sweep) openNext(ctx context.Context) (itemID, trackID int64, skipped int, err error) {
	tracks, err := sweepEligible(ctx, s.cfg.DB, s.cfg.Now(), s.cfg.Resolve != nil, sweepCandidatesPerRun)
	if err != nil {
		return 0, 0, 0, err
	}
	for _, t := range tracks {
		if err := ctx.Err(); err != nil {
			return 0, 0, skipped, err
		}
		if fi, serr := os.Stat(s.localPath(t.PlexPath)); serr != nil || !fi.Mode().IsRegular() {
			// Plex lists a file this host cannot see (moved, or the share is down). Look
			// again later; nothing is changed about the track but its schedule.
			s.log.Warn("musicengine sweep: library file not found, skipping the track", "track", t.ID, "path", t.PlexPath, "err", serr)
			if err := s.postpone(ctx, t.ID, sweepRecheckMissing); err != nil {
				return 0, 0, skipped, err
			}
			skipped++
			continue
		}
		if t.RecordingID == 0 {
			rid, rerr := s.cfg.Resolve(ctx, t)
			if rerr != nil || rid == 0 {
				s.log.Warn("musicengine sweep: could not resolve the track's recording, skipping it", "track", t.ID, "path", t.PlexPath, "err", rerr)
				if err := s.postpone(ctx, t.ID, sweepDeferAfter); err != nil {
					return 0, 0, skipped, err
				}
				skipped++
				continue
			}
			t.RecordingID = rid
		}
		id, ok, err := s.insertItem(ctx, t)
		if err != nil {
			return 0, 0, skipped, err
		}
		if !ok {
			skipped++ // another item took the recording between the list and the insert
			continue
		}
		return id, t.ID, skipped, nil
	}
	return 0, 0, skipped, nil
}

func (s *Sweep) postpone(ctx context.Context, trackID int64, d time.Duration) error {
	_, err := s.cfg.DB.ExecContext(ctx, `UPDATE mus_library_tracks SET next_sweep_at = ? WHERE id = ?`, s.cfg.Now().Add(d).Unix(), trackID)
	if err != nil {
		return fmt.Errorf("postponing sweep of track %d: %w", trackID, err)
	}
	return nil
}

// insertItem opens the item in one transaction and stamps the track with a provisional next
// visit, so a visit that ends some other way (dropped by Blair, cancelled) is not retried at
// once. The one-active-item rule is checked here and held by the database index.
func (s *Sweep) insertItem(ctx context.Context, t SweepTrack) (int64, bool, error) {
	var id int64
	taken := false
	now := s.cfg.Now()
	err := store.WithTx(ctx, s.cfg.DB, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM mus_items WHERE (recording_id = ? OR track_id = ?) AND state NOT IN ('done','cancelled','dropped')`,
			t.RecordingID, t.ID).Scan(&n); err != nil {
			return fmt.Errorf("checking active items of recording %d: %w", t.RecordingID, err)
		}
		if n > 0 {
			taken = true
			return nil
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO mus_items (kind, recording_id, track_id, state, entered_at, deadline_at, last_reason, priority, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			KindSweep, t.RecordingID, t.ID, string(StateSearching), now.Unix(), DeadlineFor(StateSearching, now),
			"upgrade sweep: looking for a better copy", PrioritySweep, now.Unix(), now.Unix())
		if err != nil {
			return fmt.Errorf("opening sweep item for track %d: %w", t.ID, err)
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE mus_library_tracks SET next_sweep_at = ? WHERE id = ?`, now.Add(s.cfg.Base).Unix(), t.ID)
		return err
	})
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return 0, false, nil // the index beat the check: someone else holds the recording
		}
		return 0, false, err
	}
	return id, !taken, nil
}

// ---- R26 schedule ----

// sweepNextInterval doubles the previous interval; the first miss uses the base. The result
// never exceeds max.
func sweepNextInterval(prev, base, max time.Duration) time.Duration {
	next := base
	if prev > 0 {
		next = prev * 2
	}
	if next < base {
		next = base
	}
	if next > max {
		next = max
	}
	return next
}

// sweepMissTrack records that a visit found nothing better: the next visit is a doubled
// interval away (R26).
func sweepMissTrack(ctx context.Context, q store.Execer, trackID int64, now time.Time, base, max time.Duration) error {
	if trackID == 0 {
		return nil
	}
	var prev int64
	if err := q.QueryRowContext(ctx, `SELECT retry_interval_s FROM mus_library_tracks WHERE id = ?`, trackID).Scan(&prev); err != nil {
		return fmt.Errorf("reading the sweep interval of track %d: %w", trackID, err)
	}
	if base <= 0 {
		base = DefaultSweepBase
	}
	if max <= 0 {
		max = DefaultSweepMax
	}
	next := sweepNextInterval(time.Duration(prev)*time.Second, base, max)
	_, err := q.ExecContext(ctx, `UPDATE mus_library_tracks SET retry_interval_s = ?, next_sweep_at = ? WHERE id = ?`,
		int64(next/time.Second), now.Add(next).Unix(), trackID)
	if err != nil {
		return fmt.Errorf("scheduling the next sweep of track %d: %w", trackID, err)
	}
	return nil
}

// sweepDeferTrack revisits the track soon without changing its retry interval.
func sweepDeferTrack(ctx context.Context, q store.Execer, trackID int64, now time.Time, after time.Duration) error {
	if trackID == 0 {
		return nil
	}
	if _, err := q.ExecContext(ctx, `UPDATE mus_library_tracks SET next_sweep_at = ? WHERE id = ?`, now.Add(after).Unix(), trackID); err != nil {
		return fmt.Errorf("deferring the sweep of track %d: %w", trackID, err)
	}
	return nil
}

// sweepResetTrack starts the schedule over after a swap: the interval is forgotten and the
// track is revisited after the base interval.
func sweepResetTrack(ctx context.Context, q store.Execer, trackID int64, now time.Time, base time.Duration) error {
	if base <= 0 {
		base = DefaultSweepBase
	}
	if _, err := q.ExecContext(ctx, `UPDATE mus_library_tracks SET retry_interval_s = 0, next_sweep_at = ? WHERE id = ?`, now.Add(base).Unix(), trackID); err != nil {
		return fmt.Errorf("resetting the sweep schedule of track %d: %w", trackID, err)
	}
	return nil
}

// ---- settling leftovers ----

// settleMisses closes a sweep item the acquirer sent to Blair with "no genuine copy". For a
// sweep that is a miss (R26), not a question: nothing better exists yet, so the track is
// retried later. Without this, every track with no better copy would reach the decision list.
func (s *Sweep) settleMisses(ctx context.Context) (int, error) {
	rows, err := s.cfg.DB.QueryContext(ctx,
		`SELECT i.id, COALESCE(i.track_id, 0), d.id
		 FROM mus_items i JOIN mus_decisions d ON d.item_id = i.id AND d.kind = ? AND d.answered_at = 0
		 WHERE i.kind = ? AND i.state = ?`, DecisionNoGenuineCopy, KindSweep, string(StateWaitingBlair))
	if err != nil {
		return 0, fmt.Errorf("listing sweep items with no better copy: %w", err)
	}
	type miss struct{ item, track, decision int64 }
	var misses []miss
	for rows.Next() {
		var m miss
		if err := rows.Scan(&m.item, &m.track, &m.decision); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scanning sweep miss: %w", err)
		}
		misses = append(misses, m)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	_ = rows.Close()

	now := s.cfg.Now()
	n := 0
	var errs []error
	for _, m := range misses {
		err := store.WithTx(ctx, s.cfg.DB, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx,
				`UPDATE mus_decisions SET answer = ?, answer_note = ?, answered_at = ? WHERE id = ? AND answered_at = 0`,
				OptionDrop, "closed automatically: the upgrade sweep found no better copy, so the track is retried later", now.Unix(), m.decision)
			if err != nil {
				return fmt.Errorf("closing decision %d: %w", m.decision, err)
			}
			if c, _ := res.RowsAffected(); c != 1 {
				return nil // answered by Blair meanwhile; leave the item to that answer
			}
			if err := swapFinishItem(ctx, tx, m.item, StateDropped, "the upgrade sweep found no better copy", now); err != nil {
				return err
			}
			return sweepMissTrack(ctx, tx, m.track, now, s.cfg.Base, s.cfg.Max)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("settling sweep item %d: %w", m.item, err))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// settleKeeps copies every "keep" answer on a held swap into the blocked table, so the
// block outlives the staged candidate's row and survives a Plex re-import (R25's key).
func (s *Sweep) settleKeeps(ctx context.Context) (int, error) {
	res, err := s.cfg.DB.ExecContext(ctx,
		`INSERT OR IGNORE INTO mus_blocked_candidates (recording_id, track_id, pcm_fingerprint, swap_id, created_at)
		 SELECT i.recording_id, i.track_id, c.pcm_fingerprint, NULL, ?
		 FROM mus_decisions d
		   JOIN mus_candidates c ON c.id = d.candidate_id
		   JOIN mus_items i ON i.id = d.item_id
		 WHERE d.answer = ? AND d.kind IN (?, ?, ?, ?) AND c.pcm_fingerprint <> '' AND i.recording_id IS NOT NULL`,
		s.cfg.Now().Unix(), OptionKeep, DecisionDifferentMaster, DecisionTradeOff, DecisionPathChange, DecisionSwapDisabled)
	if err != nil {
		return 0, fmt.Errorf("recording kept candidates as blocked: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// sweepBlockCandidate blocks one candidate's decoded audio for a recording. swapID is the
// undone swap, or 0 when Blair kept the current file.
func sweepBlockCandidate(ctx context.Context, q store.Execer, candidateID, recordingID, trackID, swapID int64, now time.Time) error {
	_, err := q.ExecContext(ctx,
		`INSERT OR IGNORE INTO mus_blocked_candidates (recording_id, track_id, pcm_fingerprint, swap_id, created_at)
		 SELECT ?, ?, pcm_fingerprint, ?, ? FROM mus_candidates WHERE id = ? AND pcm_fingerprint <> ''`,
		recordingID, nullID(trackID), nullID(swapID), now.Unix(), candidateID)
	if err != nil {
		return fmt.Errorf("blocking candidate %d for recording %d: %w", candidateID, recordingID, err)
	}
	return nil
}
