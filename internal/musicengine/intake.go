package musicengine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/tristenlammi/arrmada/internal/spotify"
	"github.com/tristenlammi/arrmada/internal/store"
)

// SyncReport is what one intake sync did.
type SyncReport struct {
	Entries   int // entries in the playlist read
	Created   int // new entries, each with a new item (R2, R3)
	Reopened  int // removed-then-re-added entries whose cancelled item came back (R3)
	Removed   int // entries that left the playlist
	Cancelled int // of those, items cancelled because no download had started (R4)
	Kept      int // of those, items left running or finished (R4)
	Skipped   int // playlist entries with nothing to identify them by
}

// alertUndelivered prefixes a stored read error whose alert the notifier refused, so the
// next failed read tries the alert again.
const alertUndelivered = "[alert not queued] "

var errEmptyRead = errors.New("playlist read returned no entries although the playlist held songs on the last sync; treating it as a failed read, not an emptied playlist")

// entryKey identifies a playlist entry by the song itself (R3): its Spotify track id. Local
// files and items with no id get a stable hash of what the playlist shows.
func entryKey(e spotify.Entry) string {
	if e.TrackID != "" {
		return e.TrackID
	}
	if e.Name == "" && len(e.Artists) == 0 && e.Album == "" && e.DurationMs == 0 {
		return ""
	}
	h := sha256.Sum256([]byte(strings.ToLower(e.Name + "\x00" + strings.Join(e.Artists, ",") + "\x00" + e.Album + "\x00" + fmt.Sprint(e.DurationMs))))
	return "local:" + hex.EncodeToString(h[:8])
}

// SyncIntake reads the watched playlist and brings the items in line with it (R1-R5).
//
// A read that fails, or cannot be proven complete, changes no item or entry: it records the
// error on the playlist row and raises one alert for the failure streak (R5). On a complete
// read, every entry the engine has not seen becomes one item, a removed-then-re-added song
// reopens its cancelled item, and a removed song cancels its item only if no download has
// started and no other present entry still points at the item (R4).
func (e *Engine) SyncIntake(ctx context.Context) (SyncReport, error) {
	var rep SyncReport
	pid := e.settings().PlaylistID
	if pid == "" {
		return rep, nil
	}
	if e.spotify == nil {
		return rep, errors.New("musicengine: no Spotify client configured")
	}
	now := e.now()
	pl, err := ensurePlaylist(ctx, e.db, pid, now)
	if err != nil {
		return rep, err
	}

	read, err := e.spotify.ReadPlaylist(ctx, pid)
	if err == nil && len(read.Entries) == 0 {
		var present int
		qerr := e.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM mus_playlist_entries WHERE playlist_id = ? AND present = 1`, pl.ID).Scan(&present)
		if qerr != nil {
			return rep, fmt.Errorf("counting present entries of playlist %s: %w", pid, qerr)
		}
		if present > 0 {
			err = errEmptyRead
		}
	}
	if err != nil {
		return rep, e.recordReadFailure(ctx, pl, err)
	}

	rep, err = e.applyRead(ctx, pl, read)
	if err != nil {
		return rep, fmt.Errorf("applying playlist %s: %w", pid, err)
	}
	return rep, nil
}

func (e *Engine) recordReadFailure(ctx context.Context, pl playlistRow, cause error) error {
	msg := clip(cause.Error(), 500)
	stored := msg
	var alertErr error
	if pl.LastError == "" || strings.HasPrefix(pl.LastError, alertUndelivered) {
		alertErr = e.notifier.Alert(ctx, Alert{
			Kind:    AlertPlaylistReadFailed,
			Key:     fmt.Sprintf("mus-playlist-read:%s:%d", pl.SpotifyID, e.now().Unix()),
			Subject: "Spotify playlist could not be read",
			Body: "The engine could not read the watched playlist completely, so it changed nothing and kept every request. Reason: " +
				msg,
		})
		if alertErr != nil {
			stored = alertUndelivered + msg
		}
	}
	if _, err := e.db.ExecContext(ctx, `UPDATE mus_playlists SET last_error = ? WHERE id = ?`, stored, pl.ID); err != nil {
		return errors.Join(cause, fmt.Errorf("recording read error on playlist %s: %w", pl.SpotifyID, err))
	}
	return errors.Join(fmt.Errorf("reading playlist %s: %w", pl.SpotifyID, cause), alertErr)
}

func (e *Engine) applyRead(ctx context.Context, pl playlistRow, read *spotify.Playlist) (SyncReport, error) {
	var rep SyncReport
	now := e.now()
	err := store.WithTx(ctx, e.db, func(tx *sql.Tx) error {
		rep = SyncReport{Entries: len(read.Entries)}
		existing, err := playlistEntries(ctx, tx, pl.ID)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, se := range read.Entries {
			key := entryKey(se)
			if key == "" {
				rep.Skipped++
				continue
			}
			if seen[key] {
				continue // the same song twice in the playlist is one request (R3)
			}
			seen[key] = true
			row, known := existing[key]
			if !known {
				if err := e.insertEntry(ctx, tx, pl.ID, key, se, now.Unix()); err != nil {
					return err
				}
				rep.Created++
				continue
			}
			if err := e.refreshEntry(ctx, tx, row, se, &rep); err != nil {
				return err
			}
		}

		var gone []entryRow
		for key, row := range existing {
			if row.Present && !seen[key] {
				gone = append(gone, row)
			}
		}
		for _, row := range gone {
			if _, err := tx.ExecContext(ctx,
				`UPDATE mus_playlist_entries SET present = 0, removed_at = ? WHERE id = ?`, now.Unix(), row.ID); err != nil {
				return fmt.Errorf("marking entry %d removed: %w", row.ID, err)
			}
			rep.Removed++
		}
		for _, row := range gone {
			if err := e.cancelIfAbandoned(ctx, tx, row, &rep); err != nil {
				return err
			}
		}

		_, err = tx.ExecContext(ctx,
			`UPDATE mus_playlists SET name = ?, owner = ?, snapshot_id = ?, last_synced_at = ?, last_error = '',
			        first_synced_at = CASE WHEN first_synced_at = 0 THEN ? ELSE first_synced_at END
			 WHERE id = ?`,
			read.Name, read.OwnerID, read.SnapshotID, now.Unix(), now.Unix(), pl.ID)
		if err != nil {
			return fmt.Errorf("recording sync of playlist %s: %w", pl.SpotifyID, err)
		}
		return nil
	})
	return rep, err
}

func (e *Engine) insertEntry(ctx context.Context, tx store.Execer, playlistID int64, key string, se spotify.Entry, nowUnix int64) error {
	itemID, err := insertItem(ctx, tx, KindRequest, PriorityIntake, 0, e.now())
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO mus_playlist_entries (playlist_id, spotify_track_id, isrc, title, artist, album,
		        duration_ms, explicit, is_local, relinked_from_id, item_id, present, first_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
		playlistID, key, se.ISRC, se.Name, strings.Join(se.Artists, ", "), se.Album, se.DurationMs,
		boolInt(se.Explicit), boolInt(isLocalEntry(se)), se.LinkedFromID, itemID, nowUnix)
	if err != nil {
		return fmt.Errorf("inserting playlist entry %s: %w", key, err)
	}
	return nil
}

// isLocalEntry: a local file, or an item Spotify gave no track id for, has no identity the
// engine can pin by.
func isLocalEntry(se spotify.Entry) bool { return se.IsLocal || se.TrackID == "" }

// refreshEntry updates a known entry from the read and, when it comes back after being
// removed, reopens its item.
func (e *Engine) refreshEntry(ctx context.Context, tx store.Execer, row entryRow, se spotify.Entry, rep *SyncReport) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE mus_playlist_entries SET isrc = ?, title = ?, artist = ?, album = ?, duration_ms = ?,
		        explicit = ?, is_local = ?, relinked_from_id = ?, present = 1, removed_at = 0
		 WHERE id = ?`,
		se.ISRC, se.Name, strings.Join(se.Artists, ", "), se.Album, se.DurationMs,
		boolInt(se.Explicit), boolInt(isLocalEntry(se)), se.LinkedFromID, row.ID)
	if err != nil {
		return fmt.Errorf("refreshing playlist entry %d: %w", row.ID, err)
	}
	if row.ItemID == 0 {
		itemID, err := insertItem(ctx, tx, KindRequest, PriorityIntake, 0, e.now())
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE mus_playlist_entries SET item_id = ? WHERE id = ?`, itemID, row.ID); err != nil {
			return fmt.Errorf("linking entry %d to item %d: %w", row.ID, itemID, err)
		}
		rep.Created++
		return nil
	}
	if row.Present {
		return nil
	}
	return e.reopenForEntry(ctx, tx, row, rep)
}

// reopenForEntry is R3's re-add: the earlier request comes back instead of a second one. If
// the cancelled item's recording is now held by another active item, the entry joins that
// item (one active item per recording, KTD3).
func (e *Engine) reopenForEntry(ctx context.Context, tx store.Execer, row entryRow, rep *SyncReport) error {
	it, err := getItem(ctx, tx, row.ItemID)
	if err != nil {
		return err
	}
	if it.State != StateCancelled {
		return nil // done, dropped or still running: nothing to reopen
	}
	if it.RecordingID != 0 {
		var holder int64
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM mus_items WHERE recording_id = ? AND id <> ? AND state NOT IN ('done','cancelled','dropped') LIMIT 1`,
			it.RecordingID, it.ID).Scan(&holder)
		if err == nil {
			if _, err := tx.ExecContext(ctx, `UPDATE mus_playlist_entries SET item_id = ? WHERE id = ?`, holder, row.ID); err != nil {
				return fmt.Errorf("pointing entry %d at item %d: %w", row.ID, holder, err)
			}
			rep.Reopened++
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("looking for an active item on recording %d: %w", it.RecordingID, err)
		}
	}
	if err := e.reopen(ctx, tx, it, "re-added to the playlist"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mus_items SET priority = ? WHERE id = ?`, PriorityIntake, it.ID); err != nil {
		return fmt.Errorf("restoring priority of item %d: %w", it.ID, err)
	}
	rep.Reopened++
	return nil
}

// cancelIfAbandoned applies R4 to an entry that just left the playlist. An item another
// present entry still points at (a duplicate entry merged into it) is kept.
func (e *Engine) cancelIfAbandoned(ctx context.Context, tx store.Execer, row entryRow, rep *SyncReport) error {
	if row.ItemID == 0 {
		return nil
	}
	var others int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mus_playlist_entries WHERE item_id = ? AND present = 1 AND id <> ?`,
		row.ItemID, row.ID).Scan(&others); err != nil {
		return fmt.Errorf("counting other entries of item %d: %w", row.ItemID, err)
	}
	if others > 0 {
		rep.Kept++
		return nil
	}
	it, err := getItem(ctx, tx, row.ItemID)
	if err != nil {
		return err
	}
	if it.State.Terminal() {
		rep.Kept++
		return nil
	}
	ok, err := canCancel(ctx, tx, it)
	if err != nil {
		return err
	}
	if !ok {
		rep.Kept++ // the download started: the item finishes, the library file is never touched (R4)
		return nil
	}
	if err := e.TransitionTx(ctx, tx, it.ID, StateCancelled, "removed from the playlist"); err != nil {
		return err
	}
	rep.Cancelled++
	return nil
}
