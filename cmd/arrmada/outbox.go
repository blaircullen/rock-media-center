package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/tristenlammi/arrmada/internal/issues"
	"github.com/tristenlammi/arrmada/internal/outbox"
	"github.com/tristenlammi/arrmada/internal/requests"
)

// importConsumers is everything that acts on an import, as outbox consumers. Each runs
// until it succeeds, so each must be safe to run twice: the inbox's unique ref stops a
// second "ready", and a second scan request for a folder is debounced into the first.
type importConsumers struct {
	requests *requests.Service
	// plex queues a debounced scan of a changed folder on every media server (Plex and
	// Jellyfin, mediaservers.go; the field and the consumer name predate Jellyfin). It
	// never blocks or fails, so the consumer is done as soon as the folder is queued, the
	// servers up or not.
	plex interface{ Request(kind, dir string) }
	// showFolder is a show's library folder ("" while it has no files).
	showFolder func(ctx context.Context, seriesID int64) string
	// plexIndexStale asks the media servers' library indexes to look again soon (nil =
	// nothing).
	plexIndexStale func()
	// issues resolves a reported problem being fixed when the new file for it lands.
	issues *issues.Service
}

// register adds every consumer. It must run before anything can import — the scheduler's
// sweeps, the HTTP manual-import routes — because Enqueue writes rows only for the
// consumers registered at that moment.
//
// Within a topic the Plex scan (which only queues a folder) and then the requester's
// "ready" are registered first: one import's rows are written in registration order and
// run oldest first, one at a time.
func (c importConsumers) register(box *outbox.Outbox) {
	c.registerPlex(box)
	box.Register(outbox.TopicMovieImported, "requests.ready", decode(func(ctx context.Context, p outbox.MovieImported) error {
		return c.requests.NotifyMovieReady(ctx, p.MovieID)
	}))
	box.Register(outbox.TopicMovieImported, "issues.resolve", decode(func(ctx context.Context, p outbox.MovieImported) error {
		if c.issues == nil {
			return nil
		}
		return c.issues.OnMovieImported(ctx, p.MovieID, p.VersionID)
	}))
	box.Register(outbox.TopicSeriesImported, "requests.ready", decode(func(ctx context.Context, p outbox.SeriesImported) error {
		return c.requests.NotifySeriesReady(ctx, p.SeriesID)
	}))
	// Only the episodes this import placed: an issue on S01E04 isn't fixed by S01E05.
	box.Register(outbox.TopicSeriesImported, "issues.resolve", decode(func(ctx context.Context, p outbox.SeriesImported) error {
		if c.issues == nil {
			return nil
		}
		eps := make([]issues.Episode, 0, len(p.Episodes))
		for _, e := range p.Episodes {
			eps = append(eps, issues.Episode{Season: e.Season, Episode: e.Episode})
		}
		return c.issues.OnSeriesImported(ctx, p.SeriesID, eps)
	}))
}

// registerPlex adds the Plex scan consumers: each queues a debounced partial scan of the
// folder that changed (plexscan), so a season pack or a burst of renames costs one scan
// per folder. Changes that never reach the outbox — series renames and deletes — call the
// scanner directly (wired in main).
func (c importConsumers) registerPlex(box *outbox.Outbox) {
	// The movie's folder: the new file, or an upgrade's replacement.
	box.Register(outbox.TopicMovieImported, "plex.scan", decode(func(_ context.Context, p outbox.MovieImported) error {
		c.plexScan("movie", p.Path)
		c.indexStale()
		return nil
	}))
	// A rename scans the old folder and the new one; a delete, the folder it left.
	box.Register(outbox.TopicMovieChanged, "plex.scan", decode(func(_ context.Context, p outbox.MovieChanged) error {
		c.plexScan("movie", p.OldPath)
		c.plexScan("movie", p.Path)
		return nil
	}))
	// One scan of the show's folder covers every episode a pack placed.
	box.Register(outbox.TopicSeriesImported, "plex.scan", decode(func(ctx context.Context, p outbox.SeriesImported) error {
		if c.plex != nil && c.showFolder != nil {
			if dir := c.showFolder(ctx, p.SeriesID); dir != "" {
				c.plex.Request("show", dir)
			}
		}
		c.indexStale()
		return nil
	}))
}

func (c importConsumers) indexStale() {
	if c.plexIndexStale != nil {
		c.plexIndexStale()
	}
}

// plexScan queues a Plex scan of the folder file is in (nothing for an empty path).
func (c importConsumers) plexScan(kind, file string) {
	if c.plex == nil || file == "" {
		return
	}
	c.plex.Request(kind, filepath.Dir(file))
}

// decode adapts a typed handler to the outbox's JSON one.
func decode[T any](h func(ctx context.Context, p T) error) outbox.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		var p T
		if err := json.Unmarshal(raw, &p); err != nil {
			return fmt.Errorf("decode payload: %w", err)
		}
		return h(ctx, p)
	}
}
