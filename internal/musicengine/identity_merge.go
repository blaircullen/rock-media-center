package musicengine

import (
	"context"
	"database/sql"
	"fmt"
)

// mergeLibraryRowKey gives the library row at path the Plex rating key a re-import assigned.
//
// The catalogue refresh runs before a remap and inserts the new key as its own row, so two
// rows can sit at one path: the old one (carrying the sweep schedule, verdict, checksum and
// every item, quarantine, journal and blocked-candidate reference) and the new one (carrying
// Plex's fresh rating, play count and playlist count). Updating the old row's key in place
// would collide with plex_rating_key's unique constraint, so the rows are merged in the
// caller's transaction: the oldest row with a different key stays canonical, the references
// of every other row at the path move to it, fresh Plex facts are copied over, the
// duplicates are deleted and the canonical row takes the new key.
//
// A key already held by a row at a different path is left alone: that is a different file.
func mergeLibraryRowKey(ctx context.Context, tx *sql.Tx, path, newKey string) error {
	var otherPath int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mus_library_tracks WHERE plex_rating_key = ? AND path <> ?`, newKey, path).Scan(&otherPath); err != nil {
		return fmt.Errorf("checking rating key %s: %w", newKey, err)
	}
	if otherPath > 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, plex_rating_key FROM mus_library_tracks WHERE path = ? ORDER BY id`, path)
	if err != nil {
		return fmt.Errorf("listing library rows at %s: %w", path, err)
	}
	type lr struct {
		id  int64
		key string
	}
	var all []lr
	for rows.Next() {
		var x lr
		if err := rows.Scan(&x.id, &x.key); err != nil {
			rows.Close()
			return err
		}
		all = append(all, x)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	var canon int64
	for _, x := range all {
		if x.key != newKey {
			canon = x.id
			break
		}
	}
	if canon == 0 {
		return nil // only the new-key row exists: already right
	}
	for _, x := range all {
		if x.id == canon {
			continue
		}
		for _, tbl := range []string{"mus_items", "mus_quarantine", "mus_swap_journal", "mus_blocked_candidates"} {
			if _, err := tx.ExecContext(ctx, `UPDATE `+tbl+` SET track_id = ? WHERE track_id = ?`, canon, x.id); err != nil {
				return fmt.Errorf("moving %s references from library row %d to %d: %w", tbl, x.id, canon, err)
			}
		}
		if x.key == newKey {
			// The new row holds what Plex just said; keep that, and the canonical row's history.
			if _, err := tx.ExecContext(ctx,
				`UPDATE mus_library_tracks SET
				    user_rating = (SELECT user_rating FROM mus_library_tracks WHERE id = ?1),
				    play_count = (SELECT play_count FROM mus_library_tracks WHERE id = ?1),
				    playlist_count = (SELECT playlist_count FROM mus_library_tracks WHERE id = ?1),
				    last_seen_at = MAX(last_seen_at, (SELECT last_seen_at FROM mus_library_tracks WHERE id = ?1)),
				    recording_id = COALESCE(recording_id, (SELECT recording_id FROM mus_library_tracks WHERE id = ?1)),
				    sha256 = CASE WHEN sha256 = '' THEN (SELECT sha256 FROM mus_library_tracks WHERE id = ?1) ELSE sha256 END
				  WHERE id = ?2`, x.id, canon); err != nil {
				return fmt.Errorf("copying fresh Plex facts from library row %d: %w", x.id, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM mus_library_tracks WHERE id = ?`, x.id); err != nil {
			return fmt.Errorf("deleting duplicate library row %d: %w", x.id, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mus_library_tracks SET plex_rating_key = ? WHERE id = ?`, newKey, canon); err != nil {
		return fmt.Errorf("giving library row %d the key %s: %w", canon, newKey, err)
	}
	return nil
}
