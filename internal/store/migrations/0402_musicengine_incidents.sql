-- 0402_musicengine_incidents: durable incident identity for the music engine's alerts, and
-- the availability flag of a playlist entry.
--
-- mus_alert_incidents: an alert's dedupe key embeds when its incident began. Held only in
-- memory, a restart minted a new key during one unbroken outage and re-alerted. The row keeps
-- the start time until the incident recovers (cleared_at set), so the key is stable across
-- restarts. Timestamps are unix seconds; 0 = not yet.
CREATE TABLE mus_alert_incidents (
    name       TEXT    PRIMARY KEY,   -- e.g. 'space-pause', 'quarantine-full'
    started_at INTEGER NOT NULL,
    cleared_at INTEGER NOT NULL DEFAULT 0
);

-- mus_playlist_entries.unavailable: 1 when Spotify listed the track but marked it not
-- playable (or the track object came back without playable data). Such an entry is routed to
-- Blair instead of being downloaded or silently kept.
ALTER TABLE mus_playlist_entries ADD COLUMN unavailable INTEGER NOT NULL DEFAULT 0;
