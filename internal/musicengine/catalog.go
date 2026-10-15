package musicengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/tristenlammi/arrmada/internal/plex"
	"github.com/tristenlammi/arrmada/internal/store"
)

// CatalogueReport is what one catalogue pass did.
type CatalogueReport struct {
	Tracks    int // tracks Plex listed
	Written   int // tracks upserted so far
	Inserted  int // rows that were new
	Skipped   int // tracks with no file path
	Playlists int // non-smart playlists counted
}

// RefreshCatalogue refreshes mus_library_tracks from Plex (KTD6): path, title, artist,
// album, rating, play count, and how many non-smart playlists hold each track (R20 orders
// the sweep by these). It reads no tags and makes no MusicBrainz call; recording ids are
// set by the on-demand library check.
//
// It is resumable by being idempotent and batch-committed: each batch is its own
// transaction and a stopped pass (cancelled context, Plex error, restart) leaves finished
// batches in place, so the next pass redoes at most the batch in flight. Playlist counts
// are read first and in full; if any playlist cannot be read the pass stops before it
// writes, so no track is left with a wrong count. Rows for tracks Plex no longer lists are
// kept (nothing is deleted); their last_seen_at shows they went missing.
func (e *Engine) RefreshCatalogue(ctx context.Context) (CatalogueReport, error) {
	var rep CatalogueReport
	if e.plex == nil {
		return rep, errors.New("musicengine: no Plex client configured")
	}
	section := e.settings().PlexSection
	if section == "" {
		return rep, errors.New("musicengine: no Plex music section configured")
	}

	counts, n, err := e.playlistCounts(ctx)
	if err != nil {
		return rep, err
	}
	rep.Playlists = n

	tracks, err := e.plex.Tracks(ctx, section)
	if err != nil {
		return rep, fmt.Errorf("listing Plex tracks for the catalogue: %w", err)
	}
	rep.Tracks = len(tracks)

	var before int
	if err := e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mus_library_tracks`).Scan(&before); err != nil {
		return rep, fmt.Errorf("counting library tracks: %w", err)
	}

	batch := e.catalogueBatch
	if batch <= 0 {
		batch = 500
	}
	now := e.now()
	for start := 0; start < len(tracks); start += batch {
		if err := ctx.Err(); err != nil {
			return rep, fmt.Errorf("catalogue pass stopped after %d of %d tracks: %w", rep.Written, rep.Tracks, err)
		}
		end := min(start+batch, len(tracks))
		skipped := 0
		err := store.WithTx(ctx, e.db, func(tx *sql.Tx) error {
			skipped = 0
			for _, t := range tracks[start:end] {
				if len(t.Files) == 0 {
					skipped++
					continue
				}
				if err := upsertCatalogueTrack(ctx, tx, t, counts[t.RatingKey], now.Unix()); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return rep, err
		}
		rep.Skipped += skipped
		rep.Written += (end - start) - skipped
		if e.afterCatalogueBatch != nil {
			e.afterCatalogueBatch(rep.Written)
		}
	}

	var after int
	if err := e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mus_library_tracks`).Scan(&after); err != nil {
		return rep, fmt.Errorf("counting library tracks: %w", err)
	}
	rep.Inserted = after - before
	return rep, nil
}

// playlistCounts maps a track's rating key to the number of non-smart playlists holding it.
func (e *Engine) playlistCounts(ctx context.Context) (map[string]int, int, error) {
	lists, err := e.plex.Playlists(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("listing Plex playlists for the catalogue: %w", err)
	}
	counts := map[string]int{}
	n := 0
	for _, p := range lists {
		if p.Smart {
			continue
		}
		items, err := e.plex.PlaylistItems(ctx, p.RatingKey)
		if err != nil {
			return nil, 0, fmt.Errorf("reading Plex playlist %q for the catalogue: %w", p.Title, err)
		}
		n++
		seen := map[string]bool{}
		for _, it := range items {
			if !seen[it.RatingKey] { // a track listed twice in one playlist is in one playlist
				seen[it.RatingKey] = true
				counts[it.RatingKey]++
			}
		}
	}
	return counts, n, nil
}

func upsertCatalogueTrack(ctx context.Context, q store.Execer, t plex.Track, playlists int, nowUnix int64) error {
	var rating any // Plex reports 0 for unrated; the column keeps NULL for that
	if t.UserRating > 0 {
		rating = t.UserRating
	}
	_, err := q.ExecContext(ctx,
		`INSERT INTO mus_library_tracks (plex_rating_key, path, title, artist, album, user_rating,
		        play_count, playlist_count, last_seen_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(plex_rating_key) DO UPDATE SET
		        verdict = CASE WHEN mus_library_tracks.path <> excluded.path THEN '' ELSE mus_library_tracks.verdict END,
		        verdict_at = CASE WHEN mus_library_tracks.path <> excluded.path THEN 0 ELSE mus_library_tracks.verdict_at END,
		        sha256 = CASE WHEN mus_library_tracks.path <> excluded.path THEN '' ELSE mus_library_tracks.sha256 END,
		        pcm_fingerprint = CASE WHEN mus_library_tracks.path <> excluded.path THEN '' ELSE mus_library_tracks.pcm_fingerprint END,
		        path = excluded.path, title = excluded.title, artist = excluded.artist, album = excluded.album,
		        user_rating = excluded.user_rating, play_count = excluded.play_count,
		        playlist_count = excluded.playlist_count, last_seen_at = excluded.last_seen_at`,
		t.RatingKey, t.Files[0], t.Title, t.Artist, t.Album, rating, t.ViewCount, playlists, nowUnix, nowUnix)
	if err != nil {
		return fmt.Errorf("upserting library track %s (%s): %w", t.RatingKey, t.Files[0], err)
	}
	return nil
}
