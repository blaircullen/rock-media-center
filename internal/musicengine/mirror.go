package musicengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/tristenlammi/arrmada/internal/plex"
	"github.com/tristenlammi/arrmada/internal/store"
)

// The Plex mirror playlist (R18): one playlist named after the watched Spotify playlist,
// holding the songs currently in it that are in the library. A song that leaves Spotify
// leaves the mirror and stays in the library. The playlist is read back after every change.

// MirrorSuffix is appended to the watched playlist's name.
const MirrorSuffix = " (mirror)"

// MirrorConfig wires a Mirror.
type MirrorConfig struct {
	DB   *sql.DB
	Plex PlexWriter
	// PlaylistID is the watched Spotify playlist id, read each time.
	PlaylistID func() string
	Now        func() time.Time
	Log        *slog.Logger
}

// Mirror keeps the playlist.
type Mirror struct {
	cfg MirrorConfig
	now func() time.Time
	log *slog.Logger
}

// NewMirror builds a Mirror.
func NewMirror(cfg MirrorConfig) (*Mirror, error) {
	if cfg.DB == nil || cfg.Plex == nil || cfg.PlaylistID == nil {
		return nil, errors.New("musicengine mirror: a database, Plex client and playlist setting are required")
	}
	m := &Mirror{cfg: cfg, now: cfg.Now, log: cfg.Log}
	if m.now == nil {
		m.now = time.Now
	}
	if m.log == nil {
		m.log = slog.New(slog.DiscardHandler)
	}
	return m, nil
}

// MirrorReport says what Sync did.
type MirrorReport struct {
	PlaylistKey string
	Title       string
	Created     bool
	Added       int
	Removed     int
	Members     int
}

type mirrorTrack struct {
	Key, Path, SHA string
	RecordingID    int64
}

// desired lists the library tracks of the watched playlist's present songs, in the order the
// songs were added. Songs without a library track yet are not members.
// A Plex re-import gives a file a new rating key and leaves the old row behind (the identity
// row keeps the recording but the catalogue refresh inserts the new key as a separate row), so
// the key comes from the row at that path the catalogue saw most recently, not the oldest one
// (Beast 2026-10-15: 41 of 56 mirror members were keys Plex answered 404 for).
func (m *Mirror) desired(ctx context.Context, playlistRow int64) ([]mirrorTrack, error) {
	rows, err := m.cfg.DB.QueryContext(ctx,
		`SELECT cur.plex_rating_key, lt.path, lt.sha256, e.recording_id
		 FROM mus_playlist_entries e
		 JOIN mus_library_tracks lt ON lt.id = (SELECT MIN(id) FROM mus_library_tracks WHERE recording_id = e.recording_id AND plex_rating_key <> '')
		 JOIN mus_library_tracks cur ON cur.id = (SELECT id FROM mus_library_tracks WHERE path = lt.path AND plex_rating_key <> '' ORDER BY last_seen_at DESC, id DESC LIMIT 1)
		 WHERE e.playlist_id = ? AND e.present = 1 AND e.recording_id IS NOT NULL
		 ORDER BY e.first_seen_at, e.id`, playlistRow)
	if err != nil {
		return nil, fmt.Errorf("listing the mirror's songs: %w", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []mirrorTrack
	for rows.Next() {
		var t mirrorTrack
		if err := rows.Scan(&t.Key, &t.Path, &t.SHA, &t.RecordingID); err != nil {
			return nil, err
		}
		if seen[t.Key] {
			continue
		}
		seen[t.Key] = true
		out = append(out, t)
	}
	return out, rows.Err()
}

// Sync brings the Plex playlist to the watched playlist's present, in-library songs and reads
// it back. Two Plex playlists with the mirror's title mean no writes and an error. With no
// members and no playlist it creates nothing (Plex cannot create an empty playlist).
func (m *Mirror) Sync(ctx context.Context) (MirrorReport, error) {
	var rep MirrorReport
	spotifyID := m.cfg.PlaylistID()
	if spotifyID == "" {
		return rep, nil
	}
	var rowID int64
	var name string
	err := m.cfg.DB.QueryRowContext(ctx, `SELECT id, name FROM mus_playlists WHERE spotify_id = ?`, spotifyID).Scan(&rowID, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return rep, nil // nothing watched yet
	}
	if err != nil {
		return rep, fmt.Errorf("reading the watched playlist: %w", err)
	}
	if strings.TrimSpace(name) == "" {
		name = "Spotify " + spotifyID
	}
	rep.Title = name + MirrorSuffix

	want, err := m.desired(ctx, rowID)
	if err != nil {
		return rep, err
	}
	wantKeys := make([]string, len(want))
	for i, t := range want {
		wantKeys[i] = t.Key
	}

	pls, err := m.cfg.Plex.Playlists(ctx)
	if err != nil {
		return rep, fmt.Errorf("mirror: listing Plex playlists: %w", err)
	}
	var hits []plex.Playlist
	for _, p := range pls {
		if p.Title == rep.Title {
			hits = append(hits, p)
		}
	}
	if len(hits) > 1 {
		return rep, fmt.Errorf("mirror: Plex has %d playlists named %q; nothing was changed", len(hits), rep.Title)
	}

	if len(hits) == 0 {
		if len(wantKeys) == 0 {
			return rep, nil
		}
		key, err := m.cfg.Plex.CreatePlaylist(ctx, rep.Title, wantKeys)
		if err != nil {
			return rep, fmt.Errorf("mirror: creating %q: %w", rep.Title, err)
		}
		rep.PlaylistKey, rep.Created, rep.Added = key, true, len(wantKeys)
	} else {
		rep.PlaylistKey = hits[0].RatingKey
		items, err := m.cfg.Plex.PlaylistItems(ctx, rep.PlaylistKey)
		if err != nil {
			return rep, fmt.Errorf("mirror: reading %q: %w", rep.Title, err)
		}
		have := map[string]bool{}
		wantSet := map[string]bool{}
		for _, k := range wantKeys {
			wantSet[k] = true
		}
		var extra []plex.PlaylistItem
		for _, it := range items {
			if !wantSet[it.RatingKey] || have[it.RatingKey] {
				extra = append(extra, it) // not a member, or a duplicate entry
				continue
			}
			have[it.RatingKey] = true
		}
		var missing []string
		for _, k := range wantKeys {
			if !have[k] {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			if err := m.cfg.Plex.AddToPlaylist(ctx, rep.PlaylistKey, missing); err != nil {
				return rep, fmt.Errorf("mirror: adding to %q: %w", rep.Title, err)
			}
			rep.Added = len(missing)
		}
		for _, it := range extra {
			if err := m.cfg.Plex.RemoveFromPlaylist(ctx, rep.PlaylistKey, it.PlaylistItemID); err != nil {
				return rep, fmt.Errorf("mirror: removing %q from %q: %w", it.Title, rep.Title, err)
			}
			rep.Removed++
		}
	}

	// Believe Plex's answer, not our request.
	items, err := m.cfg.Plex.PlaylistItems(ctx, rep.PlaylistKey)
	if err != nil {
		return rep, fmt.Errorf("mirror: reading back %q: %w", rep.Title, err)
	}
	got := make([]string, 0, len(items))
	for _, it := range items {
		got = append(got, it.RatingKey)
	}
	if !mirrorSameSet(got, wantKeys) {
		return rep, fmt.Errorf("mirror: %q reads back as %v but should hold %v", rep.Title, got, wantKeys)
	}
	rep.Members = len(got)

	if err := m.recordMemberships(ctx, rep, want); err != nil {
		return rep, err
	}
	return rep, nil
}

func mirrorSameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// recordMemberships notes the mirror membership of every current member and drops it for
// tracks that left, so a re-import can restore exactly the playlist that existed (R19).
func (m *Mirror) recordMemberships(ctx context.Context, rep MirrorReport, want []mirrorTrack) error {
	return store.WithTx(ctx, m.cfg.DB, func(tx *sql.Tx) error {
		now := m.now()
		// Replace the mirror's memberships wholesale: tracks that left, and playlist keys
		// that changed in a re-import, must not linger.
		if _, err := tx.ExecContext(ctx, `DELETE FROM mus_track_memberships WHERE is_mirror = 1 AND playlist_title = ?`, rep.Title); err != nil {
			return fmt.Errorf("clearing mirror memberships: %w", err)
		}
		for _, t := range want {
			id, err := RecordIdentity(ctx, tx, IdentityRecord{RecordingID: t.RecordingID, Path: t.Path, SHA256: t.SHA, RatingKey: t.Key}, now)
			if err != nil {
				return err
			}
			if err := RecordMembership(ctx, tx, id, rep.PlaylistKey, rep.Title, true, now); err != nil {
				return err
			}
		}
		return nil
	})
}
