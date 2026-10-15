package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tristenlammi/arrmada/internal/diskspace"
	"github.com/tristenlammi/arrmada/internal/download"
	"github.com/tristenlammi/arrmada/internal/insights"
	"github.com/tristenlammi/arrmada/internal/mediaserver"
	"github.com/tristenlammi/arrmada/internal/plex"
)

// The dashboard is one request that fans out over every subsystem, and any of them
// can be down without that being news — Plex is off, the download client is
// restarting. Each section therefore degrades to empty-with-a-note rather than
// failing the whole page, and the whole thing is bounded so a hung client can't
// hold the page open.
const dashboardTimeout = 8 * time.Second

type dashboardPayload struct {
	Storage     []storageVolume    `json:"storage"`
	Streams     *insights.Activity `json:"streams,omitempty"`
	StreamsNote string             `json:"streams_note,omitempty"`
	// PlexConfigured: a server URL and token are set. Without one there's nothing to be
	// unreachable, so StreamsNote stays empty and the page offers to connect instead.
	PlexConfigured bool `json:"plex_configured"`
	// JellyfinConfigured: a Jellyfin address and key are set. Now playing lists both
	// servers' streams, each with its server.
	JellyfinConfigured bool            `json:"jellyfin_configured"`
	Queue              queueSummary    `json:"queue"`
	QueueNote          string          `json:"queue_note,omitempty"`
	Library            libraryCounts   `json:"library"`
	Activity           []activityEvent `json:"activity"`
}

// storageVolume is one filesystem, not one folder. Several libraries usually live on
// the same array, and five identical bars say nothing five times over — Roots lists
// every configured folder that resolved to this same filesystem.
type storageVolume struct {
	Roots []string `json:"roots"`
	Path  string   `json:"path"`
	diskspace.Usage
}

type queueSummary struct {
	Downloading int   `json:"downloading"`
	Stalled     int   `json:"stalled"` // incomplete with no peer sending — not counted in Downloading
	Seeding     int   `json:"seeding"`
	Paused      int   `json:"paused"`
	Errored     int   `json:"errored"`
	DownSpeed   int64 `json:"down_speed"`
	UpSpeed     int64 `json:"up_speed"`
}

// summarizeQueue counts the client's torrents for the Dashboard tile by phase. A stalled
// torrent used to count as downloading, so a queue of dead torrents looked busy.
//
// Phases that aren't a verdict on their own (queued, checking, moving, …) go by whether the
// torrent is finished: a queued seed is still a seed.
func summarizeQueue(items []download.Item) queueSummary {
	var q queueSummary
	for _, it := range items {
		q.DownSpeed += it.DownSpeed
		q.UpSpeed += it.UpSpeed
		switch it.Phase() {
		case "error":
			q.Errored++
		case "paused":
			q.Paused++
		case "stalled":
			q.Stalled++
		case "seeding":
			q.Seeding++
		case "downloading", "metadata", "queued", "checking", "moving", "allocating":
			if it.Complete() {
				q.Seeding++
			} else {
				q.Downloading++
			}
		}
	}
	return q
}

type libraryCounts struct {
	Movies          int `json:"movies"`
	MoviesMissing   int `json:"movies_missing"`
	Series          int `json:"series"`
	Episodes        int `json:"episodes"`
	EpisodesMissing int `json:"episodes_missing"`
	Artists         int `json:"artists"`
	Albums          int `json:"albums"`
}

// activityEvent is one line of "what Rock Media Center actually did", drawn from the per-media
// event tables the detail pages already write. They were only ever readable one title
// at a time; this is the same record, unified and newest-first.
type activityEvent struct {
	Kind   string `json:"kind"` // movie | series | music (the artist)
	ID     int64  `json:"id"`
	Title  string `json:"title"`
	Event  string `json:"event"`
	Detail string `json:"detail"`
	AtMS   int64  `json:"at_ms"`
}

// streamsNote says which servers' streams are missing from Now playing ("Jellyfin: …").
func streamsNote(errs map[string]string) string {
	var parts []string
	for _, k := range []string{insights.ServerPlex, insights.ServerJellyfin} {
		if e, ok := errs[k]; ok {
			parts = append(parts, mediaserver.Kind(k).Label()+": "+e)
		}
	}
	return strings.Join(parts, "; ")
}

func (a *api) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), dashboardTimeout)
	defer cancel()

	out := dashboardPayload{
		Storage:  a.storageVolumes(),
		Library:  a.libraryCounts(ctx),
		Activity: a.recentActivity(ctx),
	}

	if a.deps.Downloads != nil {
		if items, err := a.deps.Downloads.Queue(ctx); err != nil {
			out.QueueNote = err.Error()
		} else {
			out.Queue = summarizeQueue(items)
		}
	}

	if a.deps.Insights != nil {
		cfg := a.deps.Insights.Config(ctx)
		out.PlexConfigured = cfg.URL != "" && cfg.TokenSet
		out.JellyfinConfigured = a.deps.Insights.JellyfinConfigured(ctx)
		if out.PlexConfigured || out.JellyfinConfigured {
			// Both servers' streams, each with its server; one not answering is a note
			// beside the other's streams.
			if act, err := a.deps.Insights.Activity(ctx, insights.ServerAll); err != nil {
				// The settings can be cleared between the check and the call; that is
				// still "not connected", not a failure to report.
				if errors.Is(err, plex.ErrNotConfigured) {
					out.PlexConfigured, out.JellyfinConfigured = false, false
				} else {
					out.StreamsNote = err.Error()
				}
			} else {
				out.Streams = &act
				if len(act.Errors) > 0 {
					out.StreamsNote = streamsNote(act.Errors)
				}
			}
		}
	}

	a.writeJSON(w, http.StatusOK, out)
}

// storageVolumes measures every configured root and folds the ones sharing a
// filesystem together. Two roots on one array report byte-identical totals, which is
// a cheaper and more portable identity than digging out a device id.
func (a *api) storageVolumes() []storageVolume {
	c := a.deps.Config
	type root struct{ label, path string }
	roots := []root{
		{"Movies", c.MoviesDir},
		{"TV", c.TVDir},
		{"Music", c.MusicDir},
		{"Downloads", c.DownloadsDir},
		{"App data", c.DataDir},
	}

	var out []storageVolume
	index := map[[2]uint64]int{}
	seenPath := map[string]bool{}
	for _, rt := range roots {
		if strings.TrimSpace(rt.path) == "" {
			continue
		}
		abs, err := filepath.Abs(rt.path)
		if err != nil {
			abs = rt.path
		}
		if seenPath[abs] {
			continue
		}
		seenPath[abs] = true
		u, ok := diskspace.Of(abs)
		if !ok {
			continue // unmounted, or a platform that can't measure — say nothing rather than zero
		}
		key := [2]uint64{u.TotalBytes, u.FreeBytes}
		if i, dup := index[key]; dup {
			out[i].Roots = append(out[i].Roots, rt.label)
			continue
		}
		index[key] = len(out)
		out = append(out, storageVolume{Roots: []string{rt.label}, Path: abs, Usage: u})
	}
	// Fullest first: the one about to cause a problem is the one worth seeing.
	sort.SliceStable(out, func(i, j int) bool { return out[i].UsedPct > out[j].UsedPct })
	return out
}

func (a *api) libraryCounts(ctx context.Context) libraryCounts {
	var lc libraryCounts
	if a.deps.Store == nil {
		return lc
	}
	db := a.deps.Store.DB()
	count := func(dst *int, query string) {
		var n sql.NullInt64
		if err := db.QueryRowContext(ctx, query).Scan(&n); err != nil {
			// A module whose tables aren't there yet reports zero, not an error page.
			a.deps.Log.Debug("dashboard: count failed", "query", query, "err", err)
			return
		}
		*dst = int(n.Int64)
	}
	count(&lc.Movies, `SELECT COUNT(*) FROM movies`)
	count(&lc.MoviesMissing, `SELECT COUNT(*) FROM movies WHERE monitored = 1 AND has_file = 0`)
	count(&lc.Series, `SELECT COUNT(*) FROM series`)
	count(&lc.Episodes, `SELECT COUNT(*) FROM episodes`)
	// What the searcher is actually after: aired, file-less episodes that are monitored, in
	// a monitored season of a show that isn't paused. Specials stay out, as in the series
	// list's counts. Counting every monitored row included paused shows and episodes that
	// haven't aired yet.
	count(&lc.EpisodesMissing, `SELECT COUNT(*) FROM episodes e
		JOIN series s ON s.id = e.series_id
		JOIN seasons sn ON sn.series_id = e.series_id AND sn.season_number = e.season_number
		WHERE s.monitored = 1 AND sn.monitored = 1 AND e.monitored = 1 AND e.has_file = 0
		  AND e.season_number > 0 AND e.air_date <> '' AND date(e.air_date) <= date('now')`)
	count(&lc.Artists, `SELECT COUNT(*) FROM artists`)
	count(&lc.Albums, `SELECT COUNT(*) FROM albums`)
	return lc
}

// activityLimit is what fits on the page without turning the dashboard into the Logs
// tab. The full history is still on each title's detail page.
const activityLimit = 20

func (a *api) recentActivity(ctx context.Context) []activityEvent {
	out := make([]activityEvent, 0, activityLimit)
	if a.deps.Store == nil {
		return out
	}
	// The newest slice of History's feed (every event, music included).
	events, err := eventsFeed(ctx, a.deps.Store.DB(), eventsFilter{Limit: activityLimit})
	if err != nil {
		a.deps.Log.Debug("dashboard: activity feed unavailable", "err", err)
		return out
	}
	for _, e := range events {
		out = append(out, activityEvent{Kind: e.Kind, ID: e.ItemID, Title: e.Title, Event: e.Event, Detail: e.Detail, AtMS: e.AtMS})
	}
	return out
}
