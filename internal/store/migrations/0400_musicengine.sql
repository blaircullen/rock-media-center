-- 0400_musicengine: the lossless music engine's tables (internal/musicengine).
--
-- The engine is track-level and sits beside the album module (artists/albums/tracks), so
-- every table here is prefixed mus_ and none references an album-module table.
--
-- Timestamps are unix seconds (INTEGER), 0 or NULL = not yet; the engine passes the clock
-- in, nothing here defaults to the database's. Booleans are 0/1 INTEGER.
--
-- A recording is identified by its MusicBrainz recording id (KTD2); ISRC and Spotify track
-- id are lookup keys onto it. An item can exist before its recording is pinned, so
-- mus_items.recording_id is NULL until then; SQLite treats NULLs as distinct in a unique
-- index, so unpinned items never collide with each other.
--
-- Item states are not constrained here: the state machine lives in the engine (state.go),
-- and a CHECK list would turn every new state into a table rebuild. The one rule the
-- database must hold across restarts is "one active item per recording" (KTD3), where
-- active means the state is not done, cancelled or dropped.
--
-- Migration numbers 0401-0419 stay reserved for later engine units.

-- Watched Spotify playlists (R1). snapshot_id is the last complete read's snapshot.
CREATE TABLE mus_playlists (
    id              INTEGER PRIMARY KEY,
    spotify_id      TEXT    NOT NULL UNIQUE,
    name            TEXT    NOT NULL DEFAULT '',
    owner           TEXT    NOT NULL DEFAULT '',
    snapshot_id     TEXT    NOT NULL DEFAULT '',
    enabled         INTEGER NOT NULL DEFAULT 1,
    first_synced_at INTEGER NOT NULL DEFAULT 0,
    last_synced_at  INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT    NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL
);

-- Recordings (KTD2). The mbid is the identity.
CREATE TABLE mus_recordings (
    id                 INTEGER PRIMARY KEY,
    mbid               TEXT    NOT NULL UNIQUE,
    title              TEXT    NOT NULL DEFAULT '',
    artist             TEXT    NOT NULL DEFAULT '',
    isrc               TEXT    NOT NULL DEFAULT '',
    length_ms          INTEGER NOT NULL DEFAULT 0,
    disambiguation     TEXT    NOT NULL DEFAULT '',
    version_hint       TEXT    NOT NULL DEFAULT '',  -- studio, live, acoustic, remix, edit
    explicit           INTEGER NOT NULL DEFAULT 0,
    release_group_mbid TEXT    NOT NULL DEFAULT '',
    release_mbid       TEXT    NOT NULL DEFAULT '',
    created_at         INTEGER NOT NULL
);
CREATE INDEX idx_mus_recordings_isrc ON mus_recordings(isrc) WHERE isrc <> '';

-- Library tracks, learned from Plex (KTD6). Carries what the sweep needs to order and
-- schedule (R20, R26) and what the swap comparison needs (R21, KTD19).
CREATE TABLE mus_library_tracks (
    id                 INTEGER PRIMARY KEY,
    plex_rating_key    TEXT    NOT NULL UNIQUE,
    path               TEXT    NOT NULL,
    recording_id       INTEGER REFERENCES mus_recordings(id),
    title              TEXT    NOT NULL DEFAULT '',
    artist             TEXT    NOT NULL DEFAULT '',
    album              TEXT    NOT NULL DEFAULT '',
    release_group_mbid TEXT    NOT NULL DEFAULT '',
    explicit           INTEGER NOT NULL DEFAULT 0,
    codec              TEXT    NOT NULL DEFAULT '',
    container          TEXT    NOT NULL DEFAULT '',
    bit_depth          INTEGER NOT NULL DEFAULT 0,
    sample_rate        INTEGER NOT NULL DEFAULT 0,
    channels           INTEGER NOT NULL DEFAULT 0,
    bitrate            INTEGER NOT NULL DEFAULT 0,
    duration_ms        INTEGER NOT NULL DEFAULT 0,
    verdict            TEXT    NOT NULL DEFAULT '',  -- GENUINE, SUSPECT, FAKE, UPSAMPLED, BLIND, or '' = unchecked
    verdict_at         INTEGER NOT NULL DEFAULT 0,
    spectral_top_hz    INTEGER NOT NULL DEFAULT 0,
    spectral_floor_db  REAL,
    loudness_lufs      REAL,
    pcm_fingerprint    TEXT    NOT NULL DEFAULT '',
    sha256             TEXT    NOT NULL DEFAULT '',
    user_rating        REAL,                         -- Plex 0-10 scale; NULL = unrated
    play_count         INTEGER NOT NULL DEFAULT 0,
    playlist_count     INTEGER NOT NULL DEFAULT 0,   -- non-smart Plex playlists holding it
    next_sweep_at      INTEGER NOT NULL DEFAULT 0,
    retry_interval_s   INTEGER NOT NULL DEFAULT 0,
    last_seen_at       INTEGER NOT NULL DEFAULT 0,
    created_at         INTEGER NOT NULL
);
CREATE INDEX idx_mus_library_tracks_path ON mus_library_tracks(path);
CREATE INDEX idx_mus_library_tracks_recording ON mus_library_tracks(recording_id) WHERE recording_id IS NOT NULL;
CREATE INDEX idx_mus_library_tracks_sweep ON mus_library_tracks(next_sweep_at);

-- Items (KTD4): requests and sweep visits. Each state carries a deadline (R30); paused and
-- waiting_blair suspend it, and paused_from_state remembers where to resume (R28).
CREATE TABLE mus_items (
    id                 INTEGER PRIMARY KEY,
    kind               TEXT    NOT NULL,             -- request, upgrade, sweep
    recording_id       INTEGER REFERENCES mus_recordings(id),
    track_id           INTEGER REFERENCES mus_library_tracks(id),
    state              TEXT    NOT NULL DEFAULT 'new',
    entered_at         INTEGER NOT NULL,
    deadline_at        INTEGER NOT NULL DEFAULT 0,   -- 0 = no deadline (paused, waiting, terminal)
    paused_from_state  TEXT    NOT NULL DEFAULT '',
    last_reason        TEXT    NOT NULL DEFAULT '',
    priority           INTEGER NOT NULL DEFAULT 0,   -- higher first; intake outranks the sweep (R32)
    next_attempt_at    INTEGER NOT NULL DEFAULT 0,
    retry_interval_s   INTEGER NOT NULL DEFAULT 0,
    attempts           INTEGER NOT NULL DEFAULT 0,
    alerted_state      TEXT    NOT NULL DEFAULT '',  -- state a past-deadline alert was queued for
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL
);
-- One active item per recording (KTD3, R23).
CREATE UNIQUE INDEX idx_mus_items_one_active_per_recording
    ON mus_items(recording_id)
    WHERE recording_id IS NOT NULL AND state NOT IN ('done', 'cancelled', 'dropped');
CREATE INDEX idx_mus_items_state ON mus_items(state, deadline_at);
CREATE INDEX idx_mus_items_due ON mus_items(next_attempt_at) WHERE state NOT IN ('done', 'cancelled', 'dropped');
CREATE INDEX idx_mus_items_track ON mus_items(track_id) WHERE track_id IS NOT NULL;

-- Entries of a watched playlist (R3). A removed song keeps its row (present = 0,
-- removed_at set) so a re-add reopens the same item.
CREATE TABLE mus_playlist_entries (
    id               INTEGER PRIMARY KEY,
    playlist_id      INTEGER NOT NULL REFERENCES mus_playlists(id) ON DELETE CASCADE,
    spotify_track_id TEXT    NOT NULL,
    isrc             TEXT    NOT NULL DEFAULT '',
    title            TEXT    NOT NULL DEFAULT '',
    artist           TEXT    NOT NULL DEFAULT '',
    album            TEXT    NOT NULL DEFAULT '',
    duration_ms      INTEGER NOT NULL DEFAULT 0,
    explicit         INTEGER NOT NULL DEFAULT 0,
    is_local         INTEGER NOT NULL DEFAULT 0,
    relinked_from_id TEXT    NOT NULL DEFAULT '',
    recording_id     INTEGER REFERENCES mus_recordings(id),
    item_id          INTEGER REFERENCES mus_items(id),
    present          INTEGER NOT NULL DEFAULT 1,
    first_seen_at    INTEGER NOT NULL,
    removed_at       INTEGER NOT NULL DEFAULT 0,
    UNIQUE (playlist_id, spotify_track_id)
);
CREATE INDEX idx_mus_playlist_entries_item ON mus_playlist_entries(item_id) WHERE item_id IS NOT NULL;
CREATE INDEX idx_mus_playlist_entries_recording ON mus_playlist_entries(recording_id) WHERE recording_id IS NOT NULL;

-- Candidates found for an item (R10, R12, KTD19). One row per release or file tried; a
-- rejected one stays until staging's 7-day expiry (expires_at).
CREATE TABLE mus_candidates (
    id                 INTEGER PRIMARY KEY,
    item_id            INTEGER NOT NULL REFERENCES mus_items(id) ON DELETE CASCADE,
    lane               TEXT    NOT NULL,             -- usenet, torrent, soulseek, youtube
    state              TEXT    NOT NULL DEFAULT 'found', -- found, downloading, checked, rejected, chosen
    release_name       TEXT    NOT NULL DEFAULT '',
    source_url         TEXT    NOT NULL DEFAULT '',
    client_job_id      TEXT    NOT NULL DEFAULT '',
    score              INTEGER NOT NULL DEFAULT 0,
    staging_path       TEXT    NOT NULL DEFAULT '',
    release_group_mbid TEXT    NOT NULL DEFAULT '',
    release_mbid       TEXT    NOT NULL DEFAULT '',
    verdict            TEXT    NOT NULL DEFAULT '',
    integrity_ok       INTEGER NOT NULL DEFAULT 0,
    reject_reason      TEXT    NOT NULL DEFAULT '',
    codec              TEXT    NOT NULL DEFAULT '',
    container          TEXT    NOT NULL DEFAULT '',
    bit_depth          INTEGER NOT NULL DEFAULT 0,
    sample_rate        INTEGER NOT NULL DEFAULT 0,
    channels           INTEGER NOT NULL DEFAULT 0,
    bitrate            INTEGER NOT NULL DEFAULT 0,
    duration_ms        INTEGER NOT NULL DEFAULT 0,
    decoded_ms         INTEGER NOT NULL DEFAULT 0,
    loudness_lufs      REAL,
    spectral_top_hz    INTEGER NOT NULL DEFAULT 0,
    spectral_floor_db  REAL,
    pcm_fingerprint    TEXT    NOT NULL DEFAULT '',
    sha256             TEXT    NOT NULL DEFAULT '',
    size_bytes         INTEGER NOT NULL DEFAULT 0,
    reserved_bytes     INTEGER NOT NULL DEFAULT 0,   -- space held for the download (KTD10)
    created_at         INTEGER NOT NULL,
    checked_at         INTEGER NOT NULL DEFAULT 0,
    expires_at         INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_mus_candidates_item ON mus_candidates(item_id, state);
CREATE INDEX idx_mus_candidates_expiry ON mus_candidates(expires_at) WHERE expires_at > 0;

-- Quarantined versions (KTD9, R34): every replaced file, kept separately. cleared_at is set
-- only by Blair's clear action.
CREATE TABLE mus_quarantine (
    id         INTEGER PRIMARY KEY,
    track_id   INTEGER REFERENCES mus_library_tracks(id),
    path       TEXT    NOT NULL UNIQUE,
    origin_path TEXT   NOT NULL DEFAULT '',
    sha256     TEXT    NOT NULL DEFAULT '',
    size_bytes INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    cleared_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_mus_quarantine_uncleared ON mus_quarantine(created_at) WHERE cleared_at = 0;

-- The journal of placements, swaps and undos (KTD7, KTD18). phase is the last phase
-- recorded as VERIFIED: 0 none, 1 staged, 2 temp_verified, 3 quarantine_verified,
-- 4 published, 5 plex_confirmed. A new-track placement skips phase 3. closed_at is set
-- when the row is settled (completed, rolled back or refused).
CREATE TABLE mus_swap_journal (
    id                 INTEGER PRIMARY KEY,
    kind               TEXT    NOT NULL,             -- place, swap, undo
    item_id            INTEGER REFERENCES mus_items(id),
    track_id           INTEGER REFERENCES mus_library_tracks(id),
    candidate_id       INTEGER REFERENCES mus_candidates(id),
    recording_id       INTEGER REFERENCES mus_recordings(id),
    old_path           TEXT    NOT NULL DEFAULT '',  -- incumbent (empty for a new track)
    new_path           TEXT    NOT NULL DEFAULT '',  -- destination in the library
    temp_path          TEXT    NOT NULL DEFAULT '',  -- .rmc-tmp-* name in the target folder
    quarantine_id      INTEGER REFERENCES mus_quarantine(id),
    quarantine_path    TEXT    NOT NULL DEFAULT '',
    phase              INTEGER NOT NULL DEFAULT 0,
    staged_sha256      TEXT    NOT NULL DEFAULT '',  -- phase 1
    temp_sha256        TEXT    NOT NULL DEFAULT '',  -- phase 2
    incumbent_sha256   TEXT    NOT NULL DEFAULT '',  -- what the destination must hold before phase 4
    quarantine_sha256  TEXT    NOT NULL DEFAULT '',  -- phase 3
    published_sha256   TEXT    NOT NULL DEFAULT '',  -- phase 4
    new_pcm_fingerprint TEXT   NOT NULL DEFAULT '',
    old_pcm_fingerprint TEXT   NOT NULL DEFAULT '',
    rating_before      REAL,
    memberships_before TEXT    NOT NULL DEFAULT '',  -- JSON: playlist keys recorded before the change (KTD8)
    previous_swap_id   INTEGER REFERENCES mus_swap_journal(id),
    undone_at          INTEGER NOT NULL DEFAULT 0,
    undone_by_id       INTEGER REFERENCES mus_swap_journal(id),
    last_error         TEXT    NOT NULL DEFAULT '',
    started_at         INTEGER NOT NULL,
    phase_at           INTEGER NOT NULL,
    closed_at          INTEGER NOT NULL DEFAULT 0
);
-- At most one open journal row per library path: changes to a track happen one at a time (R23).
CREATE UNIQUE INDEX idx_mus_swap_journal_one_open_per_path
    ON mus_swap_journal(new_path)
    WHERE closed_at = 0;
CREATE INDEX idx_mus_swap_journal_track ON mus_swap_journal(track_id, id);
CREATE INDEX idx_mus_swap_journal_open ON mus_swap_journal(phase) WHERE closed_at = 0;

-- Candidates an undo has blocked from returning (R25). Keyed by recording, the track's
-- durable identity, so it survives a Plex re-import that changes the track's row.
CREATE TABLE mus_blocked_candidates (
    id              INTEGER PRIMARY KEY,
    recording_id    INTEGER NOT NULL REFERENCES mus_recordings(id),
    track_id        INTEGER REFERENCES mus_library_tracks(id),
    pcm_fingerprint TEXT    NOT NULL,
    swap_id         INTEGER REFERENCES mus_swap_journal(id),
    created_at      INTEGER NOT NULL,
    UNIQUE (recording_id, pcm_fingerprint)
);

-- What the engine recorded about a track in Plex, per durable identity (recording plus
-- file), so a re-import can be remapped and read back (R19, KTD8).
CREATE TABLE mus_track_identities (
    id              INTEGER PRIMARY KEY,
    recording_id    INTEGER NOT NULL REFERENCES mus_recordings(id),
    path            TEXT    NOT NULL,
    sha256          TEXT    NOT NULL DEFAULT '',
    plex_rating_key TEXT    NOT NULL DEFAULT '',
    rating          REAL,
    recorded_at     INTEGER NOT NULL,
    remapped_at     INTEGER NOT NULL DEFAULT 0,
    UNIQUE (recording_id, path)
);
CREATE INDEX idx_mus_track_identities_key ON mus_track_identities(plex_rating_key) WHERE plex_rating_key <> '';

CREATE TABLE mus_track_memberships (
    id                INTEGER PRIMARY KEY,
    identity_id       INTEGER NOT NULL REFERENCES mus_track_identities(id) ON DELETE CASCADE,
    plex_playlist_key TEXT    NOT NULL,
    playlist_title    TEXT    NOT NULL DEFAULT '',
    is_mirror         INTEGER NOT NULL DEFAULT 0,
    recorded_at       INTEGER NOT NULL,
    UNIQUE (identity_id, plex_playlist_key)
);

-- Decisions waiting on Blair (R15, R22, KTD12). options is a JSON array of choices.
CREATE TABLE mus_decisions (
    id           INTEGER PRIMARY KEY,
    item_id      INTEGER NOT NULL REFERENCES mus_items(id) ON DELETE CASCADE,
    kind         TEXT    NOT NULL,                   -- no_genuine_copy, different_master, trade_off, path_change, ambiguous, blind_library, collision, remap, swap_disabled
    candidate_id INTEGER REFERENCES mus_candidates(id),
    journal_id   INTEGER REFERENCES mus_swap_journal(id),
    reason       TEXT    NOT NULL DEFAULT '',
    options      TEXT    NOT NULL DEFAULT '[]',
    answer       TEXT    NOT NULL DEFAULT '',
    answer_note  TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    answered_at  INTEGER NOT NULL DEFAULT 0
);
-- One open decision of a kind per item.
CREATE UNIQUE INDEX idx_mus_decisions_one_open
    ON mus_decisions(item_id, kind)
    WHERE answered_at = 0;
CREATE INDEX idx_mus_decisions_open ON mus_decisions(created_at) WHERE answered_at = 0;

-- Lane status (R8, R9, KTD20): usenet, torrent, soulseek, youtube.
CREATE TABLE mus_lane_status (
    lane          TEXT    PRIMARY KEY,
    status        TEXT    NOT NULL DEFAULT 'unknown', -- available, unavailable, unknown
    reason        TEXT    NOT NULL DEFAULT '',
    vpn_ok        INTEGER NOT NULL DEFAULT 0,
    checked_at    INTEGER NOT NULL DEFAULT 0,
    last_ok_at    INTEGER NOT NULL DEFAULT 0,
    unavailable_since INTEGER NOT NULL DEFAULT 0
);
