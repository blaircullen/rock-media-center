package musicengine

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/tristenlammi/arrmada/internal/plex"
	"github.com/tristenlammi/arrmada/internal/store"
)

// playlistItems reads a playlist's contents once per Remap run.
func (r *Remapper) playlistItems(ctx context.Context, key string) ([]plex.PlaylistItem, error) {
	if r.items == nil {
		r.items = map[string][]plex.PlaylistItem{}
	}
	if it, ok := r.items[key]; ok {
		return it, nil
	}
	it, err := r.cfg.Plex.PlaylistItems(ctx, key)
	if err != nil {
		return nil, err
	}
	r.items[key] = it
	return it, nil
}

// refreshSnapshot re-records what Plex says about a track whose rating key did not change:
// its rating, and the non-mirror playlists it sits in. The recorded memberships are replaced,
// not added to, so a playlist the user took the track out of stops being remembered. The
// mirror memberships belong to Mirror.Sync and are left alone. Nothing is written to Plex.
func (r *Remapper) refreshSnapshot(ctx context.Context, id identityRow, track plex.Track, pls []plex.Playlist) (remapOutcome, string, error) {
	ms, err := r.memberships(ctx, id.ID)
	if err != nil {
		return remapUnchanged, "", err
	}
	mirrorKeys := map[string]bool{}
	mirrorTitles := map[string]bool{}
	for _, m := range ms {
		if m.Mirror {
			mirrorKeys[m.PlaylistKey] = true
			if m.Title != "" {
				mirrorTitles[m.Title] = true
			}
		}
	}
	var in []plex.Playlist
	for _, p := range pls {
		if mirrorKeys[p.RatingKey] || mirrorTitles[p.Title] || strings.HasSuffix(p.Title, MirrorSuffix) {
			continue
		}
		items, err := r.playlistItems(ctx, p.RatingKey)
		if err != nil {
			return remapUnchanged, "", fmt.Errorf("reading playlist %q: %w", p.Title, err)
		}
		if remapHasKey(items, track.RatingKey) {
			in = append(in, p)
		}
	}
	now := r.now().Unix()
	err = store.WithTx(ctx, r.cfg.DB, func(tx *sql.Tx) error {
		var rating any
		if track.UserRating > 0 {
			rating = track.UserRating
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE mus_track_identities SET rating = ?, remapped_at = ? WHERE id = ?`, rating, now, id.ID); err != nil {
			return fmt.Errorf("refreshing rating of identity %d: %w", id.ID, err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM mus_track_memberships WHERE identity_id = ? AND is_mirror = 0`, id.ID); err != nil {
			return fmt.Errorf("clearing memberships of identity %d: %w", id.ID, err)
		}
		for _, p := range in {
			if err := RecordMembership(ctx, tx, id.ID, p.RatingKey, p.Title, false, r.now()); err != nil {
				return err
			}
		}
		return mergeLibraryRowKey(ctx, tx, id.Path, track.RatingKey)
	})
	if err != nil {
		return remapUnchanged, "", err
	}
	return remapUnchanged, "", nil
}
