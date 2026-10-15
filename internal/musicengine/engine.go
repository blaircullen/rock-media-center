// Package musicengine is the track-level lossless music engine (plan KTD1): it turns songs
// in a watched Spotify playlist into verified-genuine files in Plex, beside the album-based
// internal/music module.
//
// This core owns the item store and its durable state machine (KTD4), playlist intake, the
// pin to an exact MusicBrainz recording, the library catalogue and the on-demand library
// check. Acquisition, placement, the space gate, alerts and decisions are seams
// (interfaces.go) later units fill; each has a default so the core runs without them.
//
// Nothing here keeps item state in memory. Every state, deadline and retry time is in
// mus_items, so a restart re-enters each item's current state, and every handler is safe to
// run twice.
package musicengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tristenlammi/arrmada/internal/metadata"
	"github.com/tristenlammi/arrmada/internal/store"
)

// Settings are the engine's own settings, read fresh on every run so a change in the UI
// takes effect without a restart. U15 backs the function with settings.Service.
type Settings struct {
	// PlaylistID is the watched Spotify playlist (R1); empty disables intake.
	PlaylistID string
	// PlexSection is the Plex music section key the catalogue and library check read.
	PlexSection string
}

// Config wires an Engine. DB and Now-less defaults are enough for tests; the seams default
// to the no-op versions in interfaces.go.
type Config struct {
	DB       *sql.DB
	Settings func() Settings

	Spotify SpotifyReader
	MB      metadata.MusicRecordings
	Plex    PlexLibrary
	Checker AudioChecker
	Tags    TagReader

	Acquirer  Acquirer
	Placer    Placer
	Space     SpaceGate
	Notifier  Notifier
	Decisions DecisionRaiser

	Now func() time.Time
	Log *slog.Logger
}

// Engine is the core. Create it with New; methods are safe for concurrent use, and Tick
// itself never runs twice at once.
type Engine struct {
	db       *sql.DB
	settings func() Settings

	spotify SpotifyReader
	mb      metadata.MusicRecordings
	plex    PlexLibrary
	checker AudioChecker
	tags    TagReader

	acquirer  Acquirer
	placer    Placer
	space     SpaceGate
	notifier  Notifier
	decisions DecisionRaiser

	now func() time.Time
	log *slog.Logger

	tickMu sync.Mutex

	// catalogueBatch is how many Plex tracks one catalogue transaction writes.
	catalogueBatch int
	// afterCatalogueBatch, when set (tests), runs after each committed batch.
	afterCatalogueBatch func(done int)
}

var _ Machine = (*Engine)(nil)

// New builds an Engine. It needs a database; every other collaborator is optional at
// construction and reported by the operation that needs it.
func New(cfg Config) (*Engine, error) {
	if cfg.DB == nil {
		return nil, errors.New("musicengine: no database")
	}
	e := &Engine{
		db: cfg.DB, settings: cfg.Settings,
		spotify: cfg.Spotify, mb: cfg.MB, plex: cfg.Plex, checker: cfg.Checker, tags: cfg.Tags,
		acquirer: cfg.Acquirer, placer: cfg.Placer, space: cfg.Space, notifier: cfg.Notifier, decisions: cfg.Decisions,
		now: cfg.Now, log: cfg.Log,
		catalogueBatch: 500,
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.log == nil {
		e.log = slog.New(slog.DiscardHandler)
	}
	if e.settings == nil {
		e.settings = func() Settings { return Settings{} }
	}
	if e.acquirer == nil {
		e.acquirer = noopAcquirer{}
	}
	if e.placer == nil {
		e.placer = noopPlacer{}
	}
	if e.space == nil {
		e.space = allowAllSpace{}
	}
	if e.notifier == nil {
		e.notifier = logNotifier{log: e.log}
	}
	if e.decisions == nil {
		e.decisions = tableDecisions{}
	}
	return e, nil
}

// DB and Now satisfy Machine.
func (e *Engine) DB() *sql.DB    { return e.db }
func (e *Engine) Now() time.Time { return e.now() }

// Retry backoff for a step that returned an error (a MusicBrainz 503, a Plex timeout): the
// item stays in its state and is tried again later, so a restart or a flaky service never
// loses it. The error text stays on the item as its last reason.
const (
	stepRetryBase = 30 * time.Second
	stepRetryMax  = 30 * time.Minute
)

// batchLimit caps how many due items one Tick works on.
const batchLimit = 50

// Tick advances the engine one round: paused items whose gate has reopened resume, due
// items take one step, and past-deadline items raise their alert. The scheduler calls it
// every minute or so. A second call while one is running returns at once.
func (e *Engine) Tick(ctx context.Context) error {
	if !e.tickMu.TryLock() {
		return nil
	}
	defer e.tickMu.Unlock()

	var errs []error
	if err := e.resumePaused(ctx); err != nil {
		errs = append(errs, err)
	}
	items, err := dueItems(ctx, e.db, e.now(), batchLimit)
	if err != nil {
		errs = append(errs, err)
	}
	for _, it := range items {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		if err := e.advance(ctx, it); err != nil {
			// Already recorded on the item; one item's trouble must not stall the rest.
			e.log.Warn("musicengine step failed", "item", it.ID, "state", it.State, "err", err)
		}
	}
	if err := e.CheckDeadlines(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func workKind(it Item) WorkKind {
	if it.Kind == KindSweep {
		return WorkSweep
	}
	return WorkIntake
}

func (e *Engine) resumePaused(ctx context.Context) error {
	items, err := pausedItems(ctx, e.db)
	if err != nil {
		return err
	}
	for _, it := range items {
		ok, _ := e.space.Allow(ctx, workKind(it))
		if !ok {
			continue
		}
		if err := e.Resume(ctx, it.ID, "resumed: space available"); err != nil {
			e.log.Warn("musicengine resume failed", "item", it.ID, "err", err)
		}
	}
	return nil
}

// advance gives one due item its step. A returned error has already been recorded on the
// item (backoff and reason); the caller logs it.
func (e *Engine) advance(ctx context.Context, it Item) error {
	var err error
	switch it.State {
	case StateNew:
		err = e.Transition(ctx, it.ID, StatePinning, "starting")
	case StatePinning:
		err = e.handlePinning(ctx, it)
	case StateLibraryCheck:
		err = e.handleLibraryCheck(ctx, it)
	case StateSearching:
		if ok, why := e.space.Allow(ctx, workKind(it)); !ok {
			return e.Pause(ctx, it.ID, "paused: "+why)
		}
		err = e.acquirer.Step(ctx, e, it)
	case StateLaneUnavailable, StateDownloading, StateChecking, StateFailedCheck:
		err = e.acquirer.Step(ctx, e, it)
	case StatePlacing, StateSwapping, StateWaitingForPlayback, StateAwaitingPlexIndex:
		err = e.placer.Step(ctx, e, it)
	}
	if err != nil {
		if rerr := e.recordStepError(ctx, it, err); rerr != nil {
			return errors.Join(err, rerr)
		}
	}
	return err
}

// recordStepError keeps the evidence of a failed step on the item and backs it off.
func (e *Engine) recordStepError(ctx context.Context, it Item, cause error) error {
	cur, err := getItem(ctx, e.db, it.ID)
	if err != nil {
		return err
	}
	if cur.State != it.State {
		return nil // the step moved the item before failing; its new state has fresh bookkeeping
	}
	interval := time.Duration(cur.RetryIntervalS) * time.Second * 2
	if interval < stepRetryBase {
		interval = stepRetryBase
	}
	if interval > stepRetryMax {
		interval = stepRetryMax
	}
	now := e.now()
	_, err = e.db.ExecContext(ctx,
		`UPDATE mus_items SET attempts = attempts + 1, retry_interval_s = ?, next_attempt_at = ?,
		        last_reason = ?, updated_at = ? WHERE id = ? AND state = ?`,
		int64(interval/time.Second), now.Add(interval).Unix(), "error: "+clip(cause.Error(), 400),
		now.Unix(), it.ID, string(it.State))
	if err != nil {
		return fmt.Errorf("recording step error on item %d: %w", it.ID, err)
	}
	return nil
}

// CheckDeadlines raises one alert for each item past the deadline of its current state
// (R30). Paused and waiting_blair items have no deadline and raise none. The alert is
// marked sent only after the notifier accepted it, so a refusal is retried next run.
func (e *Engine) CheckDeadlines(ctx context.Context) error {
	now := e.now()
	rows, err := e.db.QueryContext(ctx,
		`SELECT `+itemCols+` FROM mus_items
		 WHERE deadline_at > 0 AND deadline_at <= ? AND alerted_state <> state
		   AND state NOT IN ('done','cancelled','dropped','paused','waiting_blair')
		 ORDER BY id`, now.Unix())
	if err != nil {
		return fmt.Errorf("listing items past their deadline: %w", err)
	}
	var late []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			_ = rows.Close()
			return fmt.Errorf("scanning late item: %w", err)
		}
		late = append(late, it)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	var errs []error
	for _, it := range late {
		a := Alert{
			Kind:    AlertItemPastDeadline,
			Key:     fmt.Sprintf("mus-item:%d:%s:%d", it.ID, it.State, it.EnteredAt),
			Subject: fmt.Sprintf("Music item %d is stuck in %s", it.ID, it.State),
			Body: fmt.Sprintf("Item %d (%s) has been in %s since %s, past its deadline. Last reason: %s",
				it.ID, it.Kind, it.State, time.Unix(it.EnteredAt, 0).UTC().Format(time.RFC3339), it.LastReason),
			ItemID: it.ID,
		}
		if err := e.notifier.Alert(ctx, a); err != nil {
			errs = append(errs, fmt.Errorf("alerting for item %d past deadline: %w", it.ID, err))
			continue
		}
		if _, err := e.db.ExecContext(ctx,
			`UPDATE mus_items SET alerted_state = state WHERE id = ? AND state = ? AND entered_at = ?`,
			it.ID, string(it.State), it.EnteredAt); err != nil {
			errs = append(errs, fmt.Errorf("marking item %d alerted: %w", it.ID, err))
		}
	}
	return errors.Join(errs...)
}

// raiseAndWait moves an item to waiting_blair and raises its decision in one transaction.
// Raising twice leaves one open decision (idempotent).
func (e *Engine) raiseAndWait(ctx context.Context, it Item, kind, reason string, options []string) error {
	return store.WithTx(ctx, e.db, func(tx *sql.Tx) error {
		return e.raiseAndWaitTx(ctx, tx, it, kind, reason, options)
	})
}

func (e *Engine) raiseAndWaitTx(ctx context.Context, tx store.Execer, it Item, kind, reason string, options []string) error {
	if err := e.decisions.Raise(ctx, tx, DecisionRequest{
		ItemID: it.ID, Kind: kind, Reason: reason, Options: options, Now: e.now(),
	}); err != nil {
		return err
	}
	return e.TransitionTx(ctx, tx, it.ID, StateWaitingBlair, reason)
}
