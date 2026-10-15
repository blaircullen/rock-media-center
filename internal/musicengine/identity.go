package musicengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tristenlammi/arrmada/internal/plex"
	"github.com/tristenlammi/arrmada/internal/store"
)

// Durable track identity (R19, KTD8). The engine records, per recording and file, the Plex
// rating key it knew, the rating, and the playlists the track sat in. A Plex re-import changes
// every rating key; Remap finds each track again by path and checksum, re-applies what was
// recorded, and reads it back from Plex before it believes it.
//
// Paths here are as the Plex server sees them (the same form mus_library_tracks.path uses).

// IdentityRecord is one track's durable identity. Rating is Plex's 0-10 scale; nil keeps
// whatever was recorded before.
type IdentityRecord struct {
	RecordingID int64
	Path        string
	SHA256      string
	RatingKey   string
	Rating      *float64
}

// RecordIdentity stores (or updates) the identity and returns its id. A blank checksum or key
// never overwrites a recorded one, and a nil rating leaves the recorded rating alone. q lets
// U11 record it in the same transaction as the swap.
func RecordIdentity(ctx context.Context, q store.Execer, r IdentityRecord, now time.Time) (int64, error) {
	if r.RecordingID == 0 || r.Path == "" {
		return 0, errors.New("identity needs a recording and a path")
	}
	var id int64
	err := q.QueryRowContext(ctx,
		`INSERT INTO mus_track_identities (recording_id, path, sha256, plex_rating_key, rating, recorded_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(recording_id, path) DO UPDATE SET
		        sha256 = CASE WHEN excluded.sha256 <> '' THEN excluded.sha256 ELSE mus_track_identities.sha256 END,
		        plex_rating_key = CASE WHEN excluded.plex_rating_key <> '' THEN excluded.plex_rating_key ELSE mus_track_identities.plex_rating_key END,
		        rating = CASE WHEN excluded.rating IS NOT NULL THEN excluded.rating ELSE mus_track_identities.rating END,
		        recorded_at = excluded.recorded_at
		 RETURNING id`,
		r.RecordingID, r.Path, r.SHA256, r.RatingKey, r.Rating, now.Unix()).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("recording identity of %s: %w", r.Path, err)
	}
	return id, nil
}

// RecordMembership notes that the track sat in a Plex playlist.
func RecordMembership(ctx context.Context, q store.Execer, identityID int64, playlistKey, title string, mirror bool, now time.Time) error {
	if identityID == 0 || playlistKey == "" {
		return errors.New("membership needs an identity and a playlist key")
	}
	_, err := q.ExecContext(ctx,
		`INSERT INTO mus_track_memberships (identity_id, plex_playlist_key, playlist_title, is_mirror, recorded_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(identity_id, plex_playlist_key) DO UPDATE SET playlist_title = excluded.playlist_title,
		        is_mirror = excluded.is_mirror, recorded_at = excluded.recorded_at`,
		identityID, playlistKey, title, boolInt(mirror), now.Unix())
	if err != nil {
		return fmt.Errorf("recording playlist membership %s: %w", playlistKey, err)
	}
	return nil
}

// ---- Remap ----

// RemapConfig wires a Remapper.
type RemapConfig struct {
	DB          *sql.DB
	Plex        PlexWriter
	PlexSection func() string
	// LibraryRoot and PlexRoot map identity paths (Plex's form) to files this process can
	// checksum; PlexRoot empty means the same.
	LibraryRoot, PlexRoot string
	Decisions             DecisionRaiser
	Mirror                *Mirror // restored last, after library tracks carry the new keys
	Now                   func() time.Time
	Log                   *slog.Logger
	// Hash checksums a local file; nil means sha256.
	Hash func(path string) (string, error)
}

// Remapper restores engine-recorded Plex state after a re-import.
type Remapper struct {
	cfg   RemapConfig
	paths pathMap
	log   *slog.Logger
	now   func() time.Time
	// items caches playlist contents for one Remap run (snapshot refreshes read each playlist
	// once, not once per track).
	items map[string][]plex.PlaylistItem
}

// NewRemapper builds a Remapper.
func NewRemapper(cfg RemapConfig) (*Remapper, error) {
	if cfg.DB == nil || cfg.Plex == nil || cfg.PlexSection == nil {
		return nil, errors.New("musicengine remap: a database, Plex client and section are required")
	}
	if cfg.Decisions == nil {
		cfg.Decisions = tableDecisions{}
	}
	if cfg.Hash == nil {
		cfg.Hash = placeSHA256
	}
	r := &Remapper{cfg: cfg, now: cfg.Now, log: cfg.Log}
	if r.now == nil {
		r.now = time.Now
	}
	if r.log == nil {
		r.log = slog.New(slog.DiscardHandler)
	}
	local := cfg.LibraryRoot
	r.paths = pathMap{local: local, plex: cleanOr(cfg.PlexRoot, local)}
	return r, nil
}

// RemapReport says what a remap did.
type RemapReport struct {
	Remapped   int      // tracks restored and read back
	Unchanged  int      // tracks whose key and state were already right
	Unmappable []string // "path: reason" for tracks left alone with a decision raised
	Mirror     *MirrorReport
}

type identityRow struct {
	ID          int64
	RecordingID int64
	Path        string
	SHA256      string
	Key         string
	Rating      sql.NullFloat64
}

type membershipRow struct {
	PlaylistKey, Title string
	Mirror             bool
}

// Remap walks every recorded identity. A track that cannot be found again with the same
// bytes, or that Plex lists twice, gets no writes and a decision (R19). Everything it does
// write is read back before the database records it.
func (r *Remapper) Remap(ctx context.Context) (RemapReport, error) {
	var rep RemapReport
	r.items = map[string][]plex.PlaylistItem{}
	section := r.cfg.PlexSection()
	if section == "" {
		return rep, errors.New("remap: no Plex music section is configured")
	}
	tracks, err := r.cfg.Plex.Tracks(ctx, section)
	if err != nil {
		return rep, fmt.Errorf("remap: reading Plex tracks: %w", err)
	}
	byPath := map[string][]plex.Track{}
	for _, t := range tracks {
		for _, f := range t.Files {
			byPath[f] = append(byPath[f], t)
		}
	}
	pls, err := r.cfg.Plex.Playlists(ctx)
	if err != nil {
		return rep, fmt.Errorf("remap: reading Plex playlists: %w", err)
	}
	ids, err := r.identities(ctx)
	if err != nil {
		return rep, err
	}
	var errs []error
	for _, id := range ids {
		outcome, why, err := r.remapOne(ctx, section, id, byPath, pls)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("remapping %s: %w", id.Path, err))
		case outcome == remapUnmappable:
			rep.Unmappable = append(rep.Unmappable, id.Path+": "+why)
			r.raise(ctx, id, why)
		case outcome == remapChanged:
			rep.Remapped++
			r.settle(ctx, id)
		default:
			rep.Unchanged++
			r.settle(ctx, id)
		}
	}
	if r.cfg.Mirror != nil {
		mr, err := r.cfg.Mirror.Sync(ctx)
		rep.Mirror = &mr
		if err != nil {
			errs = append(errs, fmt.Errorf("remap: restoring the mirror playlist: %w", err))
		}
	}
	return rep, errors.Join(errs...)
}

type remapOutcome int

const (
	remapUnchanged remapOutcome = iota
	remapChanged
	remapUnmappable
)

func (r *Remapper) identities(ctx context.Context) ([]identityRow, error) {
	rows, err := r.cfg.DB.QueryContext(ctx,
		`SELECT id, recording_id, path, sha256, plex_rating_key, rating FROM mus_track_identities ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("listing track identities: %w", err)
	}
	defer rows.Close()
	var out []identityRow
	for rows.Next() {
		var x identityRow
		if err := rows.Scan(&x.ID, &x.RecordingID, &x.Path, &x.SHA256, &x.Key, &x.Rating); err != nil {
			return nil, fmt.Errorf("scanning track identity: %w", err)
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (r *Remapper) memberships(ctx context.Context, identityID int64) ([]membershipRow, error) {
	rows, err := r.cfg.DB.QueryContext(ctx,
		`SELECT plex_playlist_key, playlist_title, is_mirror FROM mus_track_memberships WHERE identity_id = ? ORDER BY id`, identityID)
	if err != nil {
		return nil, fmt.Errorf("listing memberships of identity %d: %w", identityID, err)
	}
	defer rows.Close()
	var out []membershipRow
	for rows.Next() {
		var m membershipRow
		var mirror int
		if err := rows.Scan(&m.PlaylistKey, &m.Title, &mirror); err != nil {
			return nil, err
		}
		m.Mirror = mirror != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// playlistFor finds the playlist a membership meant: by title when it has one (keys change in
// a re-import), else by key.
func remapPlaylistFor(pls []plex.Playlist, m membershipRow) (plex.Playlist, string) {
	if m.Title != "" {
		var hits []plex.Playlist
		for _, p := range pls {
			if p.Title == m.Title {
				hits = append(hits, p)
			}
		}
		switch len(hits) {
		case 1:
			return hits[0], ""
		case 0:
			return plex.Playlist{}, fmt.Sprintf("The Plex playlist %q no longer exists", m.Title)
		default:
			return plex.Playlist{}, fmt.Sprintf("Plex has %d playlists named %q, so it is unclear which one this track belongs to", len(hits), m.Title)
		}
	}
	for _, p := range pls {
		if p.RatingKey == m.PlaylistKey {
			return p, ""
		}
	}
	return plex.Playlist{}, fmt.Sprintf("The Plex playlist %s no longer exists", m.PlaylistKey)
}

func (r *Remapper) remapOne(ctx context.Context, section string, id identityRow, byPath map[string][]plex.Track, pls []plex.Playlist) (remapOutcome, string, error) {
	hits := byPath[id.Path]
	switch {
	case len(hits) == 0:
		return remapUnmappable, "Plex has no track at that path", nil
	case len(hits) > 1:
		return remapUnmappable, fmt.Sprintf("Plex lists %d tracks at that path (ambiguous)", len(hits)), nil
	}
	track := hits[0]
	if id.Key == track.RatingKey {
		// Same key at the same path: Plex did not re-import anything, so Plex is the truth and
		// no checksum is needed (a library-matched track has none; asking for one raised a
		// decision on every such song, Beast 2026-10-15). Restoring the saved state here would
		// undo a rating change or playlist removal the user made since.
		return r.refreshSnapshot(ctx, id, track, pls)
	}
	// The key changed: only a checksum proves the file under the new key is the same one.
	if id.SHA256 == "" {
		return remapUnmappable, "Can't confirm this is the same file, because no checksum (a signature of its contents) was saved for it earlier.", nil
	}
	sum, err := r.cfg.Hash(r.paths.toLocal(id.Path))
	if err != nil {
		return remapUnmappable, "Couldn't read the file to confirm it: " + err.Error(), nil
	}
	if sum != id.SHA256 {
		return remapUnmappable, "The file's contents have changed since they were last checked (its checksum doesn't match), so it was left alone.", nil
	}
	ms, err := r.memberships(ctx, id.ID)
	if err != nil {
		return remapUnchanged, "", err
	}
	// Plan every write first: one unresolvable playlist means no writes for this track.
	type add struct {
		pl plex.Playlist
		m  membershipRow
	}
	var adds []add
	for _, m := range ms {
		if m.Mirror {
			continue // the mirror is restored by Mirror.Sync, which owns that playlist
		}
		pl, why := remapPlaylistFor(pls, m)
		if why != "" {
			return remapUnmappable, why, nil
		}
		adds = append(adds, add{pl: pl, m: m})
	}
	changed := true // the key moved: a verified re-import
	wantRating := id.Rating.Valid && id.Rating.Float64 > 0
	if wantRating && track.UserRating != id.Rating.Float64 {
		if err := r.cfg.Plex.SetRating(ctx, track.RatingKey, id.Rating.Float64); err != nil {
			return remapUnchanged, "", fmt.Errorf("re-applying rating: %w", err)
		}
		changed = true
	}
	for _, a := range adds {
		items, err := r.cfg.Plex.PlaylistItems(ctx, a.pl.RatingKey)
		if err != nil {
			return remapUnchanged, "", err
		}
		if remapHasKey(items, track.RatingKey) {
			continue
		}
		if err := r.cfg.Plex.AddToPlaylist(ctx, a.pl.RatingKey, []string{track.RatingKey}); err != nil {
			return remapUnchanged, "", fmt.Errorf("re-adding to playlist %q: %w", a.pl.Title, err)
		}
		changed = true
	}
	// Read back what Plex now says; only then does the database learn the new state.
	if wantRating {
		got, err := r.cfg.Plex.Tracks(ctx, section)
		if err != nil {
			return remapUnchanged, "", fmt.Errorf("reading back rating: %w", err)
		}
		ok := false
		for _, t := range got {
			if t.RatingKey == track.RatingKey && t.UserRating == id.Rating.Float64 {
				ok = true
			}
		}
		if !ok {
			return remapUnchanged, "", fmt.Errorf("rating of %s did not read back as %v", track.RatingKey, id.Rating.Float64)
		}
	}
	for _, a := range adds {
		items, err := r.cfg.Plex.PlaylistItems(ctx, a.pl.RatingKey)
		if err != nil {
			return remapUnchanged, "", err
		}
		if !remapHasKey(items, track.RatingKey) {
			return remapUnchanged, "", fmt.Errorf("track %s did not read back in playlist %q", track.RatingKey, a.pl.Title)
		}
	}
	err = store.WithTx(ctx, r.cfg.DB, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE mus_track_identities SET plex_rating_key = ?, remapped_at = ? WHERE id = ?`, track.RatingKey, r.now().Unix(), id.ID); err != nil {
			return fmt.Errorf("updating identity %d: %w", id.ID, err)
		}
		for _, a := range adds {
			if _, err := tx.ExecContext(ctx,
				`UPDATE mus_track_memberships SET plex_playlist_key = ?, playlist_title = ? WHERE identity_id = ? AND plex_playlist_key = ?`,
				a.pl.RatingKey, a.pl.Title, id.ID, a.m.PlaylistKey); err != nil {
				return fmt.Errorf("updating membership of identity %d: %w", id.ID, err)
			}
		}
		// The library row carries the key the mirror uses; merge a catalogue-refresh duplicate
		// into it and move the key.
		return mergeLibraryRowKey(ctx, tx, id.Path, track.RatingKey)
	})
	if err != nil {
		return remapUnchanged, "", err
	}
	if changed {
		return remapChanged, "", nil
	}
	return remapUnchanged, "", nil
}

func remapHasKey(items []plex.PlaylistItem, key string) bool {
	for _, it := range items {
		if it.RatingKey == key {
			return true
		}
	}
	return false
}

// settle closes an open remap decision about this track once a later run finds it fine, so a
// question that no longer applies does not wait on Blair.
func (r *Remapper) settle(ctx context.Context, id identityRow) {
	if _, err := r.cfg.DB.ExecContext(ctx,
		`UPDATE mus_decisions SET answer = 'resolved', answer_note = 'a later remap found the track fine', answered_at = ?
		 WHERE kind = 'remap' AND answered_at = 0 AND reason LIKE ? ESCAPE '\'
		   AND item_id IN (SELECT id FROM mus_items WHERE recording_id = ?)`,
		r.now().Unix(), remapLikePrefix(id.Path)+"%", id.RecordingID); err != nil {
		r.log.Warn("musicengine remap: could not close a settled decision", "path", id.Path, "err", err)
	}
}

// remapLikePrefix escapes a path for a LIKE prefix match on "<path>: ...".
func remapLikePrefix(path string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(path + ": ")
}

// raise files the remap decision on the track's newest item. Raising twice is a no-op.
func (r *Remapper) raise(ctx context.Context, id identityRow, why string) {
	itemID, err := placeLatestItemFor(ctx, r.cfg.DB, id.RecordingID)
	if err != nil || itemID == 0 {
		r.log.Warn("musicengine remap: track left alone, no item to attach a decision to", "path", id.Path, "reason", why, "err", err)
		return
	}
	if err := r.cfg.Decisions.Raise(ctx, r.cfg.DB, DecisionRequest{
		ItemID: itemID, Kind: "remap", Reason: strings.TrimSpace(id.Path + ": " + why),
		Options: []string{OptionRetry, OptionDrop}, Now: r.now(),
	}); err != nil {
		r.log.Warn("musicengine remap: could not raise decision", "path", id.Path, "err", err)
	}
}
