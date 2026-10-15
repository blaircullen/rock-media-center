package store

import (
	"context"
	"database/sql"
	"testing"
)

// 0400 creates the lossless music engine's tables. The database, not the engine, holds the
// rule that a recording has at most one active item (KTD3) and that a playlist entry is
// unique per Spotify track (R3).
func musicEngineMigrated(t *testing.T) *sql.DB {
	t.Helper()
	db := testDB(t, 1)
	if err := runMigrations(context.Background(), db, embeddedMigrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestMusicEngineMigrationCreatesTables(t *testing.T) {
	db := musicEngineMigrated(t)
	for _, tbl := range []string{
		"mus_playlists", "mus_recordings", "mus_library_tracks", "mus_items",
		"mus_playlist_entries", "mus_candidates", "mus_quarantine", "mus_swap_journal",
		"mus_blocked_candidates", "mus_track_identities", "mus_track_memberships",
		"mus_decisions", "mus_lane_status",
	} {
		if n := count(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = '`+tbl+`'`); n != 1 {
			t.Errorf("table %s: found %d, want 1", tbl, n)
		}
	}
}

func TestMusicEngineOneActiveItemPerRecording(t *testing.T) {
	db := musicEngineMigrated(t)
	if _, err := db.Exec(`INSERT INTO mus_recordings (id, mbid, created_at) VALUES (1, 'rec-1', 1), (2, 'rec-2', 1)`); err != nil {
		t.Fatal(err)
	}
	insert := func(recording any, state string) (int64, error) {
		res, err := db.Exec(`INSERT INTO mus_items (kind, recording_id, state, entered_at, created_at, updated_at) VALUES ('request', ?, ?, 1, 1, 1)`, recording, state)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}

	first, err := insert(1, "searching")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := insert(1, "new"); err == nil {
		t.Fatal("a second active item for recording 1 was accepted")
	}
	// Another recording, and items not yet pinned to a recording, are unaffected.
	if _, err := insert(2, "new"); err != nil {
		t.Errorf("active item for another recording: %v", err)
	}
	if _, err := insert(nil, "pinning"); err != nil {
		t.Errorf("first unpinned item: %v", err)
	}
	if _, err := insert(nil, "pinning"); err != nil {
		t.Errorf("second unpinned item: %v", err)
	}

	// A finished item no longer counts, whichever way it finished.
	for _, state := range []string{"done", "cancelled", "dropped"} {
		if _, err := db.Exec(`UPDATE mus_items SET state = ? WHERE id = ?`, state, first); err != nil {
			t.Fatal(err)
		}
		id, err := insert(1, "new")
		if err != nil {
			t.Fatalf("new active item after the first was %s: %v", state, err)
		}
		// Retire it again so the next round starts with no active item; first stays terminal.
		if _, err := db.Exec(`UPDATE mus_items SET state = 'done' WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}

	// Reviving a terminal item into a second active one is also refused.
	if _, err := insert(1, "new"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE mus_items SET state = 'searching' WHERE id = ?`, first); err == nil {
		t.Fatal("reopening a finished item beside an active one was accepted")
	}
}

func TestMusicEngineDuplicatePlaylistEntryFails(t *testing.T) {
	db := musicEngineMigrated(t)
	if _, err := db.Exec(`INSERT INTO mus_playlists (id, spotify_id, created_at) VALUES (1, 'pl-a', 1), (2, 'pl-b', 1)`); err != nil {
		t.Fatal(err)
	}
	add := func(playlist int, track string) error {
		_, err := db.Exec(`INSERT INTO mus_playlist_entries (playlist_id, spotify_track_id, first_seen_at) VALUES (?, ?, 1)`, playlist, track)
		return err
	}
	if err := add(1, "trk-1"); err != nil {
		t.Fatal(err)
	}
	if err := add(1, "trk-1"); err == nil {
		t.Fatal("a duplicate (playlist, spotify track id) entry was accepted")
	}
	if err := add(1, "trk-2"); err != nil {
		t.Errorf("another track in the same playlist: %v", err)
	}
	if err := add(2, "trk-1"); err != nil {
		t.Errorf("the same track in another playlist: %v", err)
	}
}

func TestMusicEngineOneOpenJournalRowPerPath(t *testing.T) {
	db := musicEngineMigrated(t)
	open := func(path string) error {
		_, err := db.Exec(`INSERT INTO mus_swap_journal (kind, new_path, started_at, phase_at) VALUES ('swap', ?, 1, 1)`, path)
		return err
	}
	if err := open("/music/A/B/01. C.flac"); err != nil {
		t.Fatal(err)
	}
	if err := open("/music/A/B/01. C.flac"); err == nil {
		t.Fatal("a second open journal row for one path was accepted")
	}
	if _, err := db.Exec(`UPDATE mus_swap_journal SET closed_at = 2`); err != nil {
		t.Fatal(err)
	}
	if err := open("/music/A/B/01. C.flac"); err != nil {
		t.Errorf("open row after the first closed: %v", err)
	}
}
