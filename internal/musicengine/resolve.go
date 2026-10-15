package musicengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tristenlammi/arrmada/internal/metadata"
	"github.com/tristenlammi/arrmada/internal/store"
)

// The sweep's recording resolver (SweepConfig.Resolve). The catalogue never sets
// mus_library_tracks.recording_id (KTD6), and the sweep needs a recording to hold the
// one-active-item rule on, so before a track is visited this reads its file tags and, when
// they do not name the recording, asks MusicBrainz in the sweep class (KTD15, R32).
//
// The order is the strongest evidence first:
//
//  1. A MusicBrainz recording id in the tags (Picard writes it). A recording already stored
//     is used without a lookup; otherwise it is fetched once and stored.
//  2. An ISRC in the tags: the recordings MusicBrainz lists under it, narrowed by the
//     title's version words, then title and length when more than one remains.
//  3. A search by artist, title and length, kept to results whose title, artist, version
//     words and length agree; a tie is broken by the track's album.
//
// A track that does not resolve to exactly one recording is skipped with the reason, never
// guessed: a wrong recording would aim an upgrade at the wrong song.

// ErrResolveSkip wraps every reason the resolver gives up on a track. The sweep logs the
// reason and tries the track again later.
var ErrResolveSkip = errors.New("musicengine: recording not resolved")

// ResolverConfig wires a Resolver.
type ResolverConfig struct {
	DB *sql.DB
	// Tags reads the library file's tags (FFprobeTags in production).
	Tags TagReader
	// MB is the shared MusicBrainz client. The resolver marks its own requests with the
	// sweep class.
	MB metadata.MusicRecordings
	// Local maps a path as Plex reports it to this process's path. Nil uses the path as is.
	Local func(plexPath string) string
	Now   func() time.Time
}

// Resolver finds the recording for a library track.
type Resolver struct{ cfg ResolverConfig }

// NewResolver validates the config.
func NewResolver(cfg ResolverConfig) (*Resolver, error) {
	if cfg.DB == nil || cfg.Tags == nil || cfg.MB == nil {
		return nil, errors.New("musicengine resolver: a database, a tag reader and a MusicBrainz client are required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Resolver{cfg: cfg}, nil
}

func resolveSkip(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrResolveSkip, fmt.Sprintf(format, args...))
}

// Resolve is SweepConfig.Resolve. It returns the recording id it wrote to the track, or 0
// and an error that wraps ErrResolveSkip.
func (r *Resolver) Resolve(ctx context.Context, t SweepTrack) (int64, error) {
	if t.RecordingID != 0 {
		return t.RecordingID, nil
	}
	ctx = metadata.WithMBClass(ctx, metadata.MBSweep)

	path := t.PlexPath
	if r.cfg.Local != nil {
		path = r.cfg.Local(path)
	}
	tags, err := r.cfg.Tags.ReadTags(ctx, path)
	if err != nil {
		return 0, fmt.Errorf("%w: reading the tags of %s: %w", ErrResolveSkip, path, err)
	}
	title := firstNonEmpty(tags.Title, t.Title)
	artist := firstNonEmpty(tags.Artist, t.Artist)
	album := firstNonEmpty(tags.Album, t.Album)
	durMs := int(tags.DurationSec * 1000)
	isrc := strings.ToUpper(strings.TrimSpace(tags.ISRC))
	en := entryRow{ISRC: isrc, Title: title, Artist: artist, Album: album, DurationMs: durMs}

	if mbid := strings.ToLower(strings.TrimSpace(tags.RecordingMBID)); mbid != "" {
		return r.fromMBID(ctx, t, mbid, en)
	}
	if title == "" {
		return 0, resolveSkip("the file has no title tag and Plex has no title for it")
	}
	if isrc != "" {
		cands, err := r.cfg.MB.RecordingsByISRC(ctx, isrc)
		if err != nil {
			return 0, fmt.Errorf("%w: musicbrainz recordings by isrc %s: %w", ErrResolveSkip, isrc, err)
		}
		if len(cands) > 0 {
			c, why := pickByISRC(cands, title, durMs)
			if c == nil {
				return 0, resolveSkip("isrc %s: %s", isrc, why)
			}
			return r.store(ctx, t.ID, *c, en)
		}
	}
	cands, err := r.cfg.MB.SearchRecordings(ctx, firstArtist(artist), title, durMs)
	if err != nil {
		return 0, fmt.Errorf("%w: musicbrainz search %q / %q: %w", ErrResolveSkip, artist, title, err)
	}
	c, why := pickBySearch(cands, artist, title, album, durMs)
	if c == nil {
		return 0, resolveSkip("search %q / %q: %s", artist, title, why)
	}
	return r.store(ctx, t.ID, *c, en)
}

// fromMBID resolves through the recording id the tags carry.
func (r *Resolver) fromMBID(ctx context.Context, t SweepTrack, mbid string, en entryRow) (int64, error) {
	var recID int64
	err := r.cfg.DB.QueryRowContext(ctx, `SELECT id FROM mus_recordings WHERE mbid = ?`, mbid).Scan(&recID)
	switch {
	case err == nil:
		return recID, r.setTrack(ctx, r.cfg.DB, t.ID, recID)
	case !errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("looking up recording %s: %w", mbid, err)
	}
	rec, err := r.cfg.MB.GetRecording(ctx, mbid)
	if err != nil {
		return 0, fmt.Errorf("%w: musicbrainz recording %s named by the file's tags: %w", ErrResolveSkip, mbid, err)
	}
	if rec == nil || rec.MBID == "" {
		return 0, resolveSkip("musicbrainz returned nothing for recording %s named by the file's tags", mbid)
	}
	return r.store(ctx, t.ID, *rec, en)
}

// store writes the recording row and points the track at it in one transaction.
func (r *Resolver) store(ctx context.Context, trackID int64, c metadata.Recording, en entryRow) (int64, error) {
	var id int64
	err := store.WithTx(ctx, r.cfg.DB, func(tx *sql.Tx) error {
		var err error
		if id, err = upsertRecording(ctx, tx, recordingRow(c, en), r.cfg.Now()); err != nil {
			return err
		}
		return r.setTrack(ctx, tx, trackID, id)
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (r *Resolver) setTrack(ctx context.Context, q store.Execer, trackID, recID int64) error {
	if _, err := q.ExecContext(ctx, `UPDATE mus_library_tracks SET recording_id = ? WHERE id = ?`, recID, trackID); err != nil {
		return fmt.Errorf("recording %d on library track %d: %w", recID, trackID, err)
	}
	return nil
}

// pickByISRC keeps the recordings under one ISRC that agree with the file's title version,
// then (when several remain) its title and length. MusicBrainz often holds several
// recordings of one master under an ISRC, so more than one left is a skip.
func pickByISRC(cands []metadata.Recording, title string, durMs int) (*metadata.Recording, string) {
	want := versionHints(title)
	var same []metadata.Recording
	for _, c := range cands {
		if sameSet(recordingVersions(c), want) {
			same = append(same, c)
		}
	}
	if len(same) == 1 {
		return &same[0], ""
	}
	if len(same) == 0 {
		return nil, fmt.Sprintf("none of the %d recordings under it has the title's version (%v)", len(cands), want)
	}
	var named []metadata.Recording
	for _, c := range same {
		if normName(c.Title) == normName(title) && lengthClose(c, durMs) {
			named = append(named, c)
		}
	}
	if len(named) == 1 {
		return &named[0], ""
	}
	return nil, fmt.Sprintf("%d recordings fit it and the title and length do not tell them apart", max(len(named), len(same)))
}

func lengthClose(c metadata.Recording, durMs int) bool {
	return c.LengthMs <= 0 || durMs <= 0 || absInt(c.LengthMs-durMs) <= durationToleranceMs
}

// pickBySearch keeps search results whose title, artist, version words and length agree
// with the file, and breaks a tie by the track's album.
func pickBySearch(cands []metadata.Recording, artist, title, album string, durMs int) (*metadata.Recording, string) {
	want := versionHints(title)
	var out []metadata.Recording
	for _, c := range cands {
		if normName(c.Title) != normName(title) || !sameSet(recordingVersions(c), want) {
			continue
		}
		if artist != "" && !artistsOverlap(c.Artist, artist) {
			continue
		}
		if !lengthClose(c, durMs) {
			continue
		}
		out = append(out, c)
	}
	switch len(out) {
	case 0:
		return nil, fmt.Sprintf("none of %d results agrees on title, artist, version and length", len(cands))
	case 1:
		return &out[0], ""
	}
	if na := normName(album); na != "" {
		var onAlbum []metadata.Recording
		for _, c := range out {
			for _, rel := range c.Releases {
				if normName(rel.Title) == na {
					onAlbum = append(onAlbum, c)
					break
				}
			}
		}
		if len(onAlbum) == 1 {
			return &onAlbum[0], ""
		}
		if len(onAlbum) > 1 {
			out = onAlbum
		}
	}
	return nil, fmt.Sprintf("%d results fit and the album does not tell them apart", len(out))
}
