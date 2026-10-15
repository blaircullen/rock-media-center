package musicengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tristenlammi/arrmada/internal/store"
)

// Item kinds.
const (
	KindRequest = "request" // a playlist request (intake)
	KindUpgrade = "upgrade" // a request whose library copy is flawed, or a sweep swap
	KindSweep   = "sweep"   // an upgrade-sweep visit
)

// Priorities: intake outranks the sweep (R32).
const (
	PriorityIntake = 100
	PrioritySweep  = 0
)

// Item is one row of mus_items. RecordingID and TrackID are 0 when unset.
type Item struct {
	ID              int64
	Kind            string
	RecordingID     int64
	TrackID         int64
	State           State
	EnteredAt       int64
	DeadlineAt      int64
	PausedFromState string
	LastReason      string
	Priority        int
	NextAttemptAt   int64
	RetryIntervalS  int64
	Attempts        int
	AlertedState    string
	CreatedAt       int64
	UpdatedAt       int64
}

const itemCols = `id, kind, COALESCE(recording_id, 0), COALESCE(track_id, 0), state, entered_at,
	deadline_at, paused_from_state, last_reason, priority, next_attempt_at, retry_interval_s,
	attempts, alerted_state, created_at, updated_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanItem(r rowScanner) (Item, error) {
	var it Item
	var state string
	err := r.Scan(&it.ID, &it.Kind, &it.RecordingID, &it.TrackID, &state, &it.EnteredAt,
		&it.DeadlineAt, &it.PausedFromState, &it.LastReason, &it.Priority, &it.NextAttemptAt,
		&it.RetryIntervalS, &it.Attempts, &it.AlertedState, &it.CreatedAt, &it.UpdatedAt)
	it.State = State(state)
	return it, err
}

// GetItem reads one item.
func (e *Engine) GetItem(ctx context.Context, id int64) (Item, error) { return getItem(ctx, e.db, id) }

func getItem(ctx context.Context, q store.Execer, id int64) (Item, error) {
	it, err := scanItem(q.QueryRowContext(ctx, `SELECT `+itemCols+` FROM mus_items WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, fmt.Errorf("%w: %d", ErrNotFound, id)
	}
	if err != nil {
		return Item{}, fmt.Errorf("reading item %d: %w", id, err)
	}
	return it, nil
}

func insertItem(ctx context.Context, q store.Execer, kind string, priority int, trackID int64, now time.Time) (int64, error) {
	var track any
	if trackID != 0 {
		track = trackID
	}
	res, err := q.ExecContext(ctx,
		`INSERT INTO mus_items (kind, track_id, state, entered_at, deadline_at, priority, created_at, updated_at)
		 VALUES (?, ?, 'new', ?, ?, ?, ?, ?)`,
		kind, track, now.Unix(), DeadlineFor(StateNew, now), priority, now.Unix(), now.Unix())
	if err != nil {
		return 0, fmt.Errorf("inserting %s item: %w", kind, err)
	}
	return res.LastInsertId()
}

// dueItems returns the items the engine should work on now: not terminal, not paused, not
// waiting for Blair, past their retry time; highest priority first, oldest first.
func dueItems(ctx context.Context, q store.Execer, now time.Time, limit int) ([]Item, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+itemCols+` FROM mus_items
		 WHERE state NOT IN ('done','cancelled','dropped','paused','waiting_blair')
		   AND next_attempt_at <= ?
		 ORDER BY priority DESC, id ASC LIMIT ?`, now.Unix(), limit)
	if err != nil {
		return nil, fmt.Errorf("listing due items: %w", err)
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning due item: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func pausedItems(ctx context.Context, q store.Execer) ([]Item, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+itemCols+` FROM mus_items WHERE state = 'paused' ORDER BY priority DESC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("listing paused items: %w", err)
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning paused item: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ---- recordings ----

// Recording is one row of mus_recordings.
type Recording struct {
	ID               int64
	MBID             string
	Title            string
	Artist           string
	ISRC             string
	LengthMs         int
	Disambiguation   string
	VersionHint      string // "studio", or a comma-joined sorted set of live, acoustic, remix, edit, demo, instrumental
	Explicit         bool
	ReleaseGroupMBID string
	ReleaseMBID      string
}

const recCols = `id, mbid, title, artist, isrc, length_ms, disambiguation, version_hint, explicit, release_group_mbid, release_mbid`

func scanRecording(r rowScanner) (Recording, error) {
	var rec Recording
	var explicit int
	err := r.Scan(&rec.ID, &rec.MBID, &rec.Title, &rec.Artist, &rec.ISRC, &rec.LengthMs,
		&rec.Disambiguation, &rec.VersionHint, &explicit, &rec.ReleaseGroupMBID, &rec.ReleaseMBID)
	rec.Explicit = explicit != 0
	return rec, err
}

func getRecording(ctx context.Context, q store.Execer, id int64) (Recording, error) {
	rec, err := scanRecording(q.QueryRowContext(ctx, `SELECT `+recCols+` FROM mus_recordings WHERE id = ?`, id))
	if err != nil {
		return Recording{}, fmt.Errorf("reading recording %d: %w", id, err)
	}
	return rec, nil
}

// upsertRecording stores a recording by its MusicBrainz id and returns its row id.
func upsertRecording(ctx context.Context, q store.Execer, r Recording, now time.Time) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx,
		`INSERT INTO mus_recordings (mbid, title, artist, isrc, length_ms, disambiguation, version_hint,
		        explicit, release_group_mbid, release_mbid, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(mbid) DO UPDATE SET title = excluded.title, artist = excluded.artist,
		        isrc = CASE WHEN excluded.isrc <> '' THEN excluded.isrc ELSE mus_recordings.isrc END,
		        length_ms = excluded.length_ms, disambiguation = excluded.disambiguation,
		        version_hint = excluded.version_hint, explicit = excluded.explicit,
		        release_group_mbid = excluded.release_group_mbid, release_mbid = excluded.release_mbid
		 RETURNING id`,
		r.MBID, r.Title, r.Artist, r.ISRC, r.LengthMs, r.Disambiguation, r.VersionHint,
		boolInt(r.Explicit), r.ReleaseGroupMBID, r.ReleaseMBID, now.Unix()).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("storing recording %s: %w", r.MBID, err)
	}
	return id, nil
}

// ---- playlists and entries ----

type playlistRow struct {
	ID            int64
	SpotifyID     string
	FirstSyncedAt int64
	LastError     string
}

func ensurePlaylist(ctx context.Context, q store.Execer, spotifyID string, now time.Time) (playlistRow, error) {
	if _, err := q.ExecContext(ctx,
		`INSERT INTO mus_playlists (spotify_id, created_at) VALUES (?, ?) ON CONFLICT(spotify_id) DO NOTHING`,
		spotifyID, now.Unix()); err != nil {
		return playlistRow{}, fmt.Errorf("registering playlist %s: %w", spotifyID, err)
	}
	var p playlistRow
	err := q.QueryRowContext(ctx,
		`SELECT id, spotify_id, first_synced_at, last_error FROM mus_playlists WHERE spotify_id = ?`, spotifyID).
		Scan(&p.ID, &p.SpotifyID, &p.FirstSyncedAt, &p.LastError)
	if err != nil {
		return playlistRow{}, fmt.Errorf("reading playlist %s: %w", spotifyID, err)
	}
	return p, nil
}

type entryRow struct {
	ID          int64
	Key         string // spotify_track_id (or a synthetic key for local files)
	ISRC        string
	Title       string
	Artist      string
	Album       string
	DurationMs  int
	Explicit    bool
	IsLocal     bool
	RelinkedID  string
	RecordingID int64
	ItemID      int64
	Present     bool
}

const entryCols = `id, spotify_track_id, isrc, title, artist, album, duration_ms, explicit, is_local,
	relinked_from_id, COALESCE(recording_id, 0), COALESCE(item_id, 0), present`

func scanEntry(r rowScanner) (entryRow, error) {
	var en entryRow
	var explicit, local, present int
	err := r.Scan(&en.ID, &en.Key, &en.ISRC, &en.Title, &en.Artist, &en.Album, &en.DurationMs,
		&explicit, &local, &en.RelinkedID, &en.RecordingID, &en.ItemID, &present)
	en.Explicit, en.IsLocal, en.Present = explicit != 0, local != 0, present != 0
	return en, err
}

func playlistEntries(ctx context.Context, q store.Execer, playlistID int64) (map[string]entryRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+entryCols+` FROM mus_playlist_entries WHERE playlist_id = ?`, playlistID)
	if err != nil {
		return nil, fmt.Errorf("listing entries of playlist %d: %w", playlistID, err)
	}
	defer rows.Close()
	out := map[string]entryRow{}
	for rows.Next() {
		en, err := scanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning playlist entry: %w", err)
		}
		out[en.Key] = en
	}
	return out, rows.Err()
}

// entryForItem is the playlist entry that asked for the item: the oldest present one, else
// the oldest at all.
func entryForItem(ctx context.Context, q store.Execer, itemID int64) (entryRow, bool, error) {
	en, err := scanEntry(q.QueryRowContext(ctx,
		`SELECT `+entryCols+` FROM mus_playlist_entries WHERE item_id = ? ORDER BY present DESC, id ASC LIMIT 1`, itemID))
	if errors.Is(err, sql.ErrNoRows) {
		return entryRow{}, false, nil
	}
	if err != nil {
		return entryRow{}, false, fmt.Errorf("reading entry of item %d: %w", itemID, err)
	}
	return en, true, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// clip shortens a stored reason; error text from a tool or server can be long.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
