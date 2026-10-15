package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tristenlammi/arrmada/internal/apikeys"
	"github.com/tristenlammi/arrmada/internal/audiocheck"
	"github.com/tristenlammi/arrmada/internal/config"
	"github.com/tristenlammi/arrmada/internal/download"
	"github.com/tristenlammi/arrmada/internal/httpapi"
	"github.com/tristenlammi/arrmada/internal/indexer"
	"github.com/tristenlammi/arrmada/internal/insights"
	"github.com/tristenlammi/arrmada/internal/libroots"
	"github.com/tristenlammi/arrmada/internal/metadata"
	"github.com/tristenlammi/arrmada/internal/musicengine"
	"github.com/tristenlammi/arrmada/internal/notify"
	"github.com/tristenlammi/arrmada/internal/plex"
	"github.com/tristenlammi/arrmada/internal/scheduler"
	"github.com/tristenlammi/arrmada/internal/settings"
	"github.com/tristenlammi/arrmada/internal/slskd"
	"github.com/tristenlammi/arrmada/internal/spotify"
	"github.com/tristenlammi/arrmada/internal/ytaudio"
)

// The lossless music engine's wiring (plan U15). main.go calls wireMusicEngine once, before
// the scheduler starts, and passes the result to httpapi as Deps.MusicEngine.
//
// The engine is off unless the setting musicengine_enabled is true when the app starts.
// Off, nothing is built, no task is registered and no engine row is touched; the returned
// API carries only the database, so the settings page can still switch the engine on (a
// restart then builds it). On, the order is the one the units document:
//
//	staging, quarantine, space gate  ->  acquire  ->  swaps  ->  placement (with swaps)
//	->  engine  ->  placement.Bind(engine)  ->  swaps.Bind(placement)  ->  Reconcile  ->  tasks
//
// Reconcile settles the placement journal before the first engine Tick (KTD7). If it fails
// (the library share is unreachable) nothing that changes engine state runs until a retry
// succeeds: every scheduler task except the digest goes through the reconciler's gate, and
// so does every mutating API route (503 with the reason). The heartbeat stays stale, which
// is what the watchdog reads.

// Setting keys. The Plex section and the two service addresses are edited on the engine
// settings page, so their names and defaults live with the API (httpapi); the Plex library
// root has no page field and is changed in the settings table.
const (
	meKeyPlexSection    = httpapi.MusicEngineKeyPlexSection
	meKeySlskdURL       = httpapi.MusicEngineKeySlskdURL
	meKeyGluetunURL     = httpapi.MusicEngineKeyGluetunURL
	meKeyPlexLibraryDir = "musicengine_plex_library_root" // the library as Plex sees it, when it differs

	meDefaultPlexSection = httpapi.MusicEngineDefaultPlexSection
	meDefaultSlskdURL    = httpapi.MusicEngineDefaultSlskdURL
)

// Scheduler intervals.
const (
	meTickEvery          = 30 * time.Second
	meIntakeEvery        = 5 * time.Minute
	meSweepEvery         = 10 * time.Minute
	meCatalogueEvery     = 24 * time.Hour
	meExpireStagingEvery = 24 * time.Hour
	meLaneCheckEvery     = 15 * time.Minute
	meSpaceCheckEvery    = 5 * time.Minute
	meDigestCheckEvery   = time.Hour
	// The digest is cut in Eastern time; it is sent on the first check at or after this hour.
	meDigestHourET = 8
)

// musicEngineDeps is what the wiring needs from main.
type musicEngineDeps struct {
	Ctx       context.Context
	Log       *slog.Logger
	Config    config.Config
	DB        *sql.DB
	Settings  *settings.Service
	Keys      *apikeys.Store
	Roots     *libroots.Roots
	Sched     *scheduler.Scheduler
	MB        *metadata.MusicBrainz
	Insights  *insights.Service
	Indexers  *indexer.Service
	Downloads *download.Service
	Notify    *notify.Service
}

// wireMusicEngine builds the engine when it is switched on and returns what the HTTP layer
// needs. It never stops the app: a failure is logged at error level and the engine stays
// off, because the rest of Rock Media Center does not depend on it.
func wireMusicEngine(d musicEngineDeps) *httpapi.MusicEngineAPI {
	off := &httpapi.MusicEngineAPI{DB: d.DB}
	if !d.Settings.GetBool(d.Ctx, httpapi.MusicEngineKeyEnabled, false) {
		d.Log.Info("music engine is switched off (setting musicengine_enabled); switch it on in the Music engine settings and restart")
		return off
	}
	api, err := buildMusicEngine(d)
	if err != nil {
		d.Log.Error("music engine could not start; it stays off", "err", err)
		return off
	}
	return api
}

func buildMusicEngine(d musicEngineDeps) (*httpapi.MusicEngineAPI, error) {
	ctx, log := d.Ctx, d.Log

	libRoot, err := filepath.Abs(d.Roots.Music(ctx))
	if err != nil || d.Roots.Music(ctx) == "" {
		return nil, fmt.Errorf("the music library folder %q is not usable: %v", d.Roots.Music(ctx), err)
	}
	dlRoot, err := filepath.Abs(d.Roots.Downloads(ctx))
	if err != nil || d.Roots.Downloads(ctx) == "" {
		return nil, fmt.Errorf("the downloads folder %q is not usable: %v", d.Roots.Downloads(ctx), err)
	}
	plexRoot := strings.TrimSpace(d.Settings.Get(ctx, meKeyPlexLibraryDir, ""))
	stagingRoot := filepath.Join(dlRoot, "rmc", "staging")
	quarantineRoot := filepath.Join(dlRoot, "rmc", "quarantine")
	for _, dir := range []string{stagingRoot, quarantineRoot} {
		if err := os.MkdirAll(dir, 0o775); err != nil {
			return nil, fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	cfgSettings := func() httpapi.MusicEngineSettings {
		return httpapi.LoadMusicEngineSettings(context.Background(), d.Settings)
	}
	plexSection := func() string {
		return strings.TrimSpace(d.Settings.Get(context.Background(), meKeyPlexSection, meDefaultPlexSection))
	}
	gb := func(v float64) int64 { return int64(v * float64(1<<30)) }

	notifier := musicengine.NewNotifier(d.Notify)
	checker := audiocheck.New()
	tags := musicengine.FFprobeTags{}
	plexLive := livePlex{ins: d.Insights}

	staging, err := musicengine.NewStaging(stagingRoot, libRoot)
	if err != nil {
		return nil, err
	}
	space, err := musicengine.NewSpace(musicengine.SpaceConfig{
		DB: d.DB, LibraryPath: libRoot, DownloadsPath: dlRoot, Notifier: notifier, Log: modLog(log, "musicengine"),
		Settings: func() musicengine.SpaceSettings {
			s := cfgSettings()
			return musicengine.SpaceSettings{Floor: gb(s.FloorGB), Margin: gb(s.MarginGB), QuarantineShare: gb(s.QuarantineShareGB)}
		},
	})
	if err != nil {
		return nil, err
	}
	quarantine, err := musicengine.NewQuarantine(musicengine.QuarantineConfig{
		DB: d.DB, Root: quarantineRoot, Log: modLog(log, "musicengine"),
		Share: func() int64 { return gb(cfgSettings().QuarantineShareGB) },
	})
	if err != nil {
		return nil, err
	}

	vpn := musicengine.NewGluetunVPN(musicengine.GluetunVPNConfig{
		URL: func() string {
			return d.Settings.Get(context.Background(), meKeyGluetunURL, musicengine.DefaultGluetunControlURL)
		},
		APIKey: d.Keys.Func(apikeys.GluetunControl),
	})
	acquire, err := musicengine.NewAcquire(musicengine.AcquireConfig{
		DB:          d.DB,
		Indexer:     d.Indexers,
		Downloads:   d.Downloads,
		Slskd:       &liveSlskd{settings: d.Settings, keys: d.Keys},
		YouTube:     &ytaudio.Client{},
		Checker:     checker,
		Space:       space,
		VPN:         vpn.Check,
		Staging:     staging,
		SoulseekDir: filepath.Join(dlRoot, "slskd", "complete"), // slskd.yml directories.downloads on Beast; incomplete/ is beside it
		Log:         modLog(log, "musicengine"),
	})
	if err != nil {
		return nil, err
	}

	swaps, err := musicengine.NewSwaps(musicengine.SwapsConfig{
		Checker: checker, Plex: plexLive, Quarantine: quarantine,
		AutoSwaps: func() bool { return httpapi.MusicEngineAutoSwaps(context.Background(), d.Settings) },
		Log:       modLog(log, "musicengine"),
	})
	if err != nil {
		return nil, err
	}
	mirror, err := musicengine.NewMirror(musicengine.MirrorConfig{
		DB: d.DB, Plex: plexLive, PlaylistID: func() string { return cfgSettings().WatchedPlaylist }, Log: modLog(log, "musicengine"),
	})
	if err != nil {
		return nil, err
	}
	placement, err := musicengine.NewPlacement(musicengine.PlacementConfig{
		LibraryRoot: libRoot, PlexRoot: plexRoot, QuarantineRoot: quarantineRoot,
		PlexSection: plexSection, Plex: plexLive, Checker: checker, Tags: tags,
		Editor: musicengine.Metaflac{}, Releases: d.MB, Staging: staging, Mirror: mirror,
		Swaps: swaps, Space: space, Finalize: swaps.Finalize, Log: modLog(log, "musicengine"),
	})
	if err != nil {
		return nil, err
	}

	engine, err := musicengine.New(musicengine.Config{
		DB: d.DB,
		Settings: func() musicengine.Settings {
			return musicengine.Settings{PlaylistID: cfgSettings().WatchedPlaylist, PlexSection: plexSection()}
		},
		Spotify: &liveSpotify{keys: d.Keys, notBefore: d.Settings}, MB: d.MB, Plex: plexLive, Checker: checker, Tags: tags,
		Acquirer: acquire, Placer: placement, Space: space, Notifier: notifier,
		Log: modLog(log, "musicengine"),
	})
	if err != nil {
		return nil, err
	}
	placement.Bind(engine)
	if err := swaps.Bind(placement); err != nil {
		return nil, err
	}

	album, err := musicengine.NewAlbum(musicengine.AlbumConfig{Placement: placement, Releases: d.MB, Staging: staging, Library: engine})
	if err != nil {
		return nil, err
	}
	remapper, err := musicengine.NewRemapper(musicengine.RemapConfig{
		DB: d.DB, Plex: plexLive, PlexSection: plexSection, LibraryRoot: libRoot, PlexRoot: plexRoot,
		Mirror: mirror, Log: modLog(log, "musicengine"),
	})
	if err != nil {
		return nil, err
	}
	resolver, err := musicengine.NewResolver(musicengine.ResolverConfig{
		DB: d.DB, Tags: tags, MB: d.MB, Local: plexToLocal(plexRoot, libRoot),
	})
	if err != nil {
		return nil, err
	}
	sweep, err := musicengine.NewSweep(musicengine.SweepConfig{
		DB: d.DB, Space: space, Swaps: swaps, Resolve: resolver.Resolve, Log: modLog(log, "musicengine"),
	})
	if err != nil {
		return nil, err
	}
	decisions := musicengine.NewDecisions(engine, musicengine.AnswerHooks{
		TakeLossy: func(ctx context.Context, dec musicengine.Decision, it musicengine.Item) error {
			return acquire.AcceptLossy(ctx, engine, it.ID)
		},
		ApproveSwap: swaps.ApproveSwap,
		TreatAsMatch: func(ctx context.Context, _ musicengine.Decision, it musicengine.Item) error {
			return engine.TreatLibraryFileAsMatch(ctx, it.ID)
		},
	})
	heartbeat := musicengine.NewHeartbeat(nil)
	digest := musicengine.NewDigest(d.DB, notifier, nil)

	// Reconcile before anything runs (KTD7). A failure is retried by the Tick task.
	rec := &reconciler{reconcile: placement.Reconcile, log: modLog(log, "musicengine")}
	if err := rec.ensure(d.Ctx); err != nil {
		log.Error("music engine: reconcile failed; no engine step runs until it succeeds", "err", err)
	}

	registerMusicEngineTasks(d, rec, engine, heartbeat, sweep, acquire, digest, space, remapper, mirror)
	log.Info("music engine is on",
		"library", libRoot, "staging", stagingRoot, "quarantine", quarantineRoot, "plex_section", plexSection(),
		"playlist", cfgSettings().WatchedPlaylist)

	return &httpapi.MusicEngineAPI{
		DB: d.DB, Decisions: decisions, Swaps: swaps, Quarantine: quarantine, Space: space,
		Album: album, Remapper: remapper, Heartbeat: heartbeat, Checker: checker,
		LibraryRoot: func() string { return libRoot }, LibraryPath: libRoot, DownloadsPath: dlRoot,
		Ready: rec.ensure, Running: true,
	}, nil
}

// reconciler runs Placement.Reconcile until it has succeeded once.
type reconciler struct {
	reconcile func(context.Context) (musicengine.ReconcileReport, error)
	log       *slog.Logger
	mu        sync.Mutex
	done      bool
}

func (r *reconciler) ensure(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return nil
	}
	rep, err := r.reconcile(ctx)
	r.log.Info("music engine: reconciled the placement journal",
		"rows", rep.Rows, "rolled_back", rep.RolledBack, "stopped", rep.Stopped, "advanced", rep.Advanced,
		"closed", rep.Closed, "confirmed_by_plex", rep.ConfirmedBy, "err", err)
	if err != nil {
		return err
	}
	r.done = true
	return nil
}

// gate wraps a task body so it runs only once the startup reconcile has succeeded; until
// then it retries the reconcile and returns the reason, doing nothing else.
func (r *reconciler) gate(run func(ctx context.Context) error) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if err := r.ensure(ctx); err != nil {
			return fmt.Errorf("the placement journal is not settled yet: %w", err)
		}
		return run(ctx)
	}
}

func registerMusicEngineTasks(d musicEngineDeps, rec *reconciler, engine *musicengine.Engine, hb *musicengine.Heartbeat,
	sweep *musicengine.Sweep, acquire *musicengine.Acquire, digest *musicengine.DigestSender, space *musicengine.Space,
	remapper *musicengine.Remapper, mirror *musicengine.Mirror) {
	s, log := d.Sched, modLog(d.Log, "musicengine")
	notifier := musicengine.NewNotifier(d.Notify)

	// Every task that reads or changes engine state waits for the startup reconcile
	// (finding 16); a failed reconcile is retried here and the task does nothing else.
	gated := rec.gate

	s.Register("musicengine-tick", meTickEvery, true, gated(func(ctx context.Context) error {
		return hb.Tick(ctx, engine)
	}), scheduler.Label("Music engine: advance requests"),
		scheduler.Description("Moves each requested or upgraded song one step through the music engine. A successful run is the engine's heartbeat."))

	// The mirror playlist follows the watched playlist, so it is brought up to date after
	// every successful intake run, not only in the daily catalogue task (finding 19).
	s.Register("musicengine-intake", meIntakeEvery, true, gated(func(ctx context.Context) error {
		rep, err := engine.SyncIntake(ctx)
		if rep.Created > 0 || rep.Removed > 0 || rep.Reopened > 0 {
			log.Info("music engine: playlist synced", "entries", rep.Entries, "created", rep.Created, "reopened", rep.Reopened, "removed", rep.Removed)
		}
		if err != nil {
			return err
		}
		if _, err := mirror.Sync(ctx); err != nil {
			return fmt.Errorf("playlist mirror: %w", err)
		}
		return nil
	}), scheduler.Label("Music engine: read the Spotify playlist"),
		scheduler.Description("Reads the watched Spotify playlist, opens a request for each new song and updates the mirror playlist."))

	// The catalogue first: Plex rating keys go stale after a re-import, and the remap and
	// the playlist mirror both depend on fresh keys.
	s.Register("musicengine-catalogue", meCatalogueEvery, true, gated(func(ctx context.Context) error {
		rep, err := engine.RefreshCatalogue(ctx)
		if err != nil {
			return err
		}
		log.Info("music engine: catalogue refreshed", "tracks", rep.Tracks, "inserted", rep.Inserted, "playlists", rep.Playlists)
		var errs []error
		if _, err := remapper.Remap(ctx); err != nil {
			errs = append(errs, fmt.Errorf("remap: %w", err))
		}
		if _, err := mirror.Sync(ctx); err != nil {
			errs = append(errs, fmt.Errorf("playlist mirror: %w", err))
		}
		return errors.Join(errs...)
	}), scheduler.Label("Music engine: refresh the library catalogue"),
		scheduler.Description("Reads every music track from Plex, restores ratings and playlists after a re-import, and updates the mirror playlist."))

	s.Register("musicengine-sweep", meSweepEvery, false, gated(func(ctx context.Context) error {
		rep, err := sweep.Run(ctx)
		if rep.Created > 0 || rep.Paused {
			log.Info("music engine: upgrade sweep", "created_item", rep.Created, "track", rep.Track, "paused", rep.Paused, "reason", rep.Reason, "skipped", rep.Skipped)
		}
		return err
	}), scheduler.Label("Music engine: upgrade sweep"),
		scheduler.Description("Picks the next library song to look for a better copy of, and finishes any swap or undo left half-done."))

	s.Register("musicengine-expire-staging", meExpireStagingEvery, true, gated(func(ctx context.Context) error {
		rep, err := acquire.ExpireStaging(ctx)
		log.Info("music engine: staging expiry", "report", fmt.Sprintf("%+v", rep), "err", err)
		return err
	}), scheduler.Label("Music engine: clear old downloads"),
		scheduler.Description("Removes staged downloads that were rejected or never used, after a week."))

	s.Register("musicengine-lane-check", meLaneCheckEvery, false, gated(func(ctx context.Context) error {
		return musicengine.CheckLanes(ctx, d.DB, notifier, time.Now())
	}), scheduler.Label("Music engine: check the sources"),
		scheduler.Description("Alerts when a music source (usenet, torrents, Soulseek) has been unavailable for more than a day."))

	s.Register("musicengine-space-check", meSpaceCheckEvery, true, gated(func(ctx context.Context) error {
		_, err := space.Status(ctx)
		return err
	}), scheduler.Label("Music engine: check disk space"),
		scheduler.Description("Measures the library and downloads drives so the engine pauses, resumes and alerts on free space even while idle."))

	et, err := time.LoadLocation("America/New_York")
	if err != nil {
		log.Warn("music engine: America/New_York zone data missing, cutting the digest hour in UTC", "err", err)
		et = time.UTC
	}
	s.Register("musicengine-digest", meDigestCheckEvery, false, func(ctx context.Context) error {
		if time.Now().In(et).Hour() < meDigestHourET {
			return nil
		}
		sent, err := digest.Send(ctx)
		if sent {
			log.Info("music engine: daily digest queued")
		}
		return err
	}, scheduler.Label("Music engine: daily digest"),
		scheduler.Description("Queues one summary of the last day's music engine activity, from 8 AM Eastern."))
}

// plexToLocal maps a path as Plex reports it to this process's path. With no separate Plex
// root the path is returned unchanged.
func plexToLocal(plexRoot, local string) func(string) string {
	plexRoot = filepath.Clean(plexRoot)
	if strings.TrimSpace(plexRoot) == "" || plexRoot == "." || plexRoot == filepath.Clean(local) {
		return func(p string) string { return p }
	}
	return func(p string) string {
		rel, err := filepath.Rel(plexRoot, p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return p
		}
		return filepath.Join(local, rel)
	}
}

// ---- adapters: clients built from settings on each use, so a key or address saved after
// start applies without a restart ----

var errPlexNotSet = errors.New("Plex is not connected (Settings → Media servers needs a server address and token)")

// livePlex is the Plex client as configured right now.
type livePlex struct{ ins *insights.Service }

func (l livePlex) client(ctx context.Context) (*plex.Client, error) {
	if !l.ins.Configured(ctx) {
		return nil, errPlexNotSet
	}
	return l.ins.PlexClient(ctx), nil
}

func (l livePlex) Tracks(ctx context.Context, section string) ([]plex.Track, error) {
	c, err := l.client(ctx)
	if err != nil {
		return nil, err
	}
	return c.Tracks(ctx, section)
}

func (l livePlex) SearchTracks(ctx context.Context, section, artist, title string) ([]plex.Track, error) {
	c, err := l.client(ctx)
	if err != nil {
		return nil, err
	}
	return c.SearchTracks(ctx, section, artist, title)
}

func (l livePlex) Playlists(ctx context.Context) ([]plex.Playlist, error) {
	c, err := l.client(ctx)
	if err != nil {
		return nil, err
	}
	return c.Playlists(ctx)
}

func (l livePlex) PlaylistItems(ctx context.Context, key string) ([]plex.PlaylistItem, error) {
	c, err := l.client(ctx)
	if err != nil {
		return nil, err
	}
	return c.PlaylistItems(ctx, key)
}

func (l livePlex) RefreshPath(ctx context.Context, section, dir string) error {
	c, err := l.client(ctx)
	if err != nil {
		return err
	}
	return c.RefreshPath(ctx, section, dir)
}

func (l livePlex) CreatePlaylist(ctx context.Context, title string, ratingKeys []string) (string, error) {
	c, err := l.client(ctx)
	if err != nil {
		return "", err
	}
	return c.CreatePlaylist(ctx, title, ratingKeys)
}

func (l livePlex) AddToPlaylist(ctx context.Context, key string, ratingKeys []string) error {
	c, err := l.client(ctx)
	if err != nil {
		return err
	}
	return c.AddToPlaylist(ctx, key, ratingKeys)
}

func (l livePlex) RemoveFromPlaylist(ctx context.Context, key, itemID string) error {
	c, err := l.client(ctx)
	if err != nil {
		return err
	}
	return c.RemoveFromPlaylist(ctx, key, itemID)
}

func (l livePlex) SetRating(ctx context.Context, ratingKey string, rating float64) error {
	c, err := l.client(ctx)
	if err != nil {
		return err
	}
	return c.SetRating(ctx, ratingKey, rating)
}

func (l livePlex) Sessions(ctx context.Context) ([]plex.Session, error) {
	c, err := l.client(ctx)
	if err != nil {
		return nil, err
	}
	return c.Sessions(ctx)
}

var (
	_ musicengine.PlexLibrary = livePlex{}
	_ musicengine.PlacePlex   = livePlex{}
	_ musicengine.SwapPlex    = livePlex{}
)

// liveSpotify builds the Spotify client from the saved credentials, rebuilding it when they
// change. Without credentials every read fails with spotify.ErrNotConfigured, which the
// engine records and alerts on like any failed read (R5).
type liveSpotify struct {
	keys *apikeys.Store
	// notBefore keeps Spotify's Retry-After cool-down across restarts (Astra #22).
	notBefore spotify.NotBeforeStore

	mu         sync.Mutex
	cur        *spotify.Client
	id, secret string
}

func (l *liveSpotify) ReadPlaylist(ctx context.Context, playlistID string) (*spotify.Playlist, error) {
	id, secret := l.keys.Value(ctx, apikeys.SpotifyClientID), l.keys.Value(ctx, apikeys.SpotifyClientSecret)
	l.mu.Lock()
	if l.cur == nil || id != l.id || secret != l.secret {
		c, err := spotify.New(spotify.Config{ClientID: id, ClientSecret: secret, NotBefore: l.notBefore})
		if err != nil {
			l.mu.Unlock()
			return nil, fmt.Errorf("Spotify client ID and secret are not both set (Settings → Connections): %w", err)
		}
		l.cur, l.id, l.secret = c, id, secret
	}
	c := l.cur
	l.mu.Unlock()
	return c.ReadPlaylist(ctx, playlistID)
}

// liveSlskd builds the slskd client from the saved address and key on each call; it is
// cheap, and slskd search state lives in slskd, not in the client.
type liveSlskd struct {
	settings *settings.Service
	keys     *apikeys.Store
}

func (l *liveSlskd) client(ctx context.Context) (*slskd.Client, error) {
	key := l.keys.Value(ctx, apikeys.SlskdAPIKey)
	if key == "" {
		return nil, errors.New("the slskd API key is not set (Settings → Connections)")
	}
	return slskd.New(l.settings.Get(ctx, meKeySlskdURL, meDefaultSlskdURL), key), nil
}

func (l *liveSlskd) Search(ctx context.Context, query string, opt slskd.SearchOptions) ([]slskd.File, error) {
	c, err := l.client(ctx)
	if err != nil {
		return nil, err
	}
	return c.Search(ctx, query, opt)
}

func (l *liveSlskd) Enqueue(ctx context.Context, username, filename string, size int64) error {
	c, err := l.client(ctx)
	if err != nil {
		return err
	}
	return c.Enqueue(ctx, username, filename, size)
}

func (l *liveSlskd) GetDownload(ctx context.Context, username, filename string) (slskd.Transfer, error) {
	c, err := l.client(ctx)
	if err != nil {
		return slskd.Transfer{}, err
	}
	return c.GetDownload(ctx, username, filename)
}

var _ musicengine.LaneSlskd = (*liveSlskd)(nil)
