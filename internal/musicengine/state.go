package musicengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tristenlammi/arrmada/internal/store"
)

// State is an item's lifecycle state, stored as these exact lowercase names (schema
// conventions, U1).
type State string

const (
	StateNew                State = "new"
	StatePinning            State = "pinning"
	StateLibraryCheck       State = "library_check"
	StateSearching          State = "searching"
	StateLaneUnavailable    State = "lane_unavailable"
	StateDownloading        State = "downloading"
	StateChecking           State = "checking"
	StateFailedCheck        State = "failed_check"
	StatePlacing            State = "placing"
	StateSwapping           State = "swapping"
	StateWaitingForPlayback State = "waiting_for_playback"
	StateAwaitingPlexIndex  State = "awaiting_plex_index"
	StateWaitingBlair       State = "waiting_blair"
	StatePaused             State = "paused"
	StateDone               State = "done"
	StateCancelled          State = "cancelled"
	StateDropped            State = "dropped"
)

// Terminal reports whether the state ends an item.
func (s State) Terminal() bool {
	return s == StateDone || s == StateCancelled || s == StateDropped
}

// Per-state deadlines (R30). paused, waiting_blair and the terminal states have none
// (KTD4): a long pause or an unanswered decision raises one alert, not one per day.
const (
	DeadlineNew                = 10 * time.Minute
	DeadlinePinning            = 1 * time.Hour
	DeadlineLibraryCheck       = 2 * time.Hour
	DeadlineSearching          = 24 * time.Hour
	DeadlineLaneUnavailable    = 24 * time.Hour
	DeadlineDownloading        = 6 * time.Hour
	DeadlineChecking           = 1 * time.Hour
	DeadlineFailedCheck        = 1 * time.Hour
	DeadlinePlacing            = 1 * time.Hour
	DeadlineSwapping           = 1 * time.Hour
	DeadlineWaitingForPlayback = 6 * time.Hour
	DeadlineAwaitingPlexIndex  = 1 * time.Hour
)

var deadlines = map[State]time.Duration{
	StateNew:                DeadlineNew,
	StatePinning:            DeadlinePinning,
	StateLibraryCheck:       DeadlineLibraryCheck,
	StateSearching:          DeadlineSearching,
	StateLaneUnavailable:    DeadlineLaneUnavailable,
	StateDownloading:        DeadlineDownloading,
	StateChecking:           DeadlineChecking,
	StateFailedCheck:        DeadlineFailedCheck,
	StatePlacing:            DeadlinePlacing,
	StateSwapping:           DeadlineSwapping,
	StateWaitingForPlayback: DeadlineWaitingForPlayback,
	StateAwaitingPlexIndex:  DeadlineAwaitingPlexIndex,
}

// DeadlineFor is the unix-second deadline for an item entering state at now, 0 for a state
// that suspends it.
func DeadlineFor(s State, now time.Time) int64 {
	d, ok := deadlines[s]
	if !ok {
		return 0
	}
	return now.Add(d).Unix()
}

// transitions is the state diagram. The edges marked (+) are not drawn in the plan's
// diagram but its prose needs them: the journal's "item returns to searching", U10's
// collision decision and U11's "stops the item with that reason".
var transitions = map[State][]State{
	StateNew:                {StatePinning},
	StatePinning:            {StateLibraryCheck, StateWaitingBlair},
	StateLibraryCheck:       {StateDone, StateSearching, StateWaitingBlair},
	StateSearching:          {StateLaneUnavailable, StateDownloading, StateWaitingBlair},
	StateLaneUnavailable:    {StateSearching},
	StateDownloading:        {StateChecking, StateSearching}, // (+) searching: a failed download tries the next candidate
	StateChecking:           {StateFailedCheck, StatePlacing, StateSwapping, StateWaitingBlair},
	StateFailedCheck:        {StateSearching, StateWaitingBlair},                                                                // (+) waiting_blair: every candidate failed
	StateSwapping:           {StateWaitingForPlayback, StateAwaitingPlexIndex, StateWaitingBlair, StateSearching, StateDropped}, // (+) dropped: the sweep keeps the incumbent
	StateWaitingForPlayback: {StateSwapping, StateDropped},                                                                      // (+) dropped: deferred after the playback wait
	StatePlacing:            {StateAwaitingPlexIndex, StateWaitingBlair, StateSearching},                                        // (+) both
	StateAwaitingPlexIndex:  {StateDone, StateWaitingBlair},                                                                     // (+) waiting_blair: remap
	// (+) pinning and library_check: Blair settled an ambiguous recording or file.
	StateWaitingBlair: {StateSearching, StatePlacing, StateSwapping, StateDropped, StatePinning, StateLibraryCheck},
}

// preDownload are the states from which an item may be cancelled (R4). waiting_blair is in
// the set but is only cancellable while no candidate has left 'found' (see downloadStarted).
var preDownload = map[State]bool{
	StateNew: true, StatePinning: true, StateLibraryCheck: true, StateSearching: true,
	StateLaneUnavailable: true, StateWaitingBlair: true,
}

// Errors the state machine returns.
var (
	ErrNotFound          = errors.New("musicengine: item not found")
	ErrInvalidTransition = errors.New("musicengine: invalid state transition")
	// ErrNotCancellable means a download has started (or the state is past it); R4 lets the
	// item finish.
	ErrNotCancellable = errors.New("musicengine: item can no longer be cancelled")
)

func allowed(from, to State, pausedFrom State) bool {
	if from.Terminal() {
		return false
	}
	if to == StatePaused {
		return from != StatePaused
	}
	if from == StatePaused {
		return to == pausedFrom || to == StateCancelled
	}
	if to == StateCancelled {
		return preDownload[from]
	}
	for _, t := range transitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// writeState is the one place an item's state columns are written: state, entered_at,
// deadline_at and last_reason together, with the retry bookkeeping reset for the new state.
func writeState(ctx context.Context, q store.Execer, it Item, to State, reason string, now time.Time) error {
	pausedFrom := ""
	if to == StatePaused {
		pausedFrom = string(it.State)
	}
	res, err := q.ExecContext(ctx,
		`UPDATE mus_items SET state = ?, entered_at = ?, deadline_at = ?, paused_from_state = ?,
		        last_reason = ?, next_attempt_at = 0, attempts = 0, retry_interval_s = 0,
		        alerted_state = '', updated_at = ?
		 WHERE id = ?`,
		string(to), now.Unix(), DeadlineFor(to, now), pausedFrom, reason, now.Unix(), it.ID)
	if err != nil {
		return fmt.Errorf("writing item %d state %s -> %s: %w", it.ID, it.State, to, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("writing item %d state %s -> %s: %d rows changed", it.ID, it.State, to, n)
	}
	return nil
}

// downloadStarted reports whether any candidate of the item has left 'found'. From then on
// the item finishes; removal never cancels it (R4, AE11).
func downloadStarted(ctx context.Context, q store.Execer, itemID int64) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mus_candidates WHERE item_id = ? AND state <> 'found'`, itemID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("checking whether item %d started downloading: %w", itemID, err)
	}
	return n > 0, nil
}

// canCancel reports whether the item can be cancelled right now.
func canCancel(ctx context.Context, q store.Execer, it Item) (bool, error) {
	eff := it.State
	if eff == StatePaused {
		eff = State(it.PausedFromState)
	}
	if !preDownload[eff] {
		return false, nil
	}
	started, err := downloadStarted(ctx, q, it.ID)
	if err != nil {
		return false, err
	}
	return !started, nil
}

// Transition moves one item to a new state in its own transaction.
func (e *Engine) Transition(ctx context.Context, itemID int64, to State, reason string) error {
	return store.WithTx(ctx, e.db, func(tx *sql.Tx) error {
		return e.TransitionTx(ctx, tx, itemID, to, reason)
	})
}

// TransitionTx moves one item inside the caller's transaction. Moving to the state the item
// is already in changes nothing (handlers are idempotent). A resume (paused -> the state it
// left) starts that state's deadline afresh.
func (e *Engine) TransitionTx(ctx context.Context, tx store.Execer, itemID int64, to State, reason string) error {
	it, err := getItem(ctx, tx, itemID)
	if err != nil {
		return err
	}
	if it.State == to {
		return nil
	}
	if !allowed(it.State, to, State(it.PausedFromState)) {
		return fmt.Errorf("%w: item %d %s -> %s", ErrInvalidTransition, itemID, it.State, to)
	}
	if to == StateCancelled {
		ok, err := canCancel(ctx, tx, it)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: item %d in %s", ErrNotCancellable, itemID, it.State)
		}
	}
	return writeState(ctx, tx, it, to, reason, e.Now())
}

// Pause suspends an item's deadline and remembers where to resume (R28, KTD4).
func (e *Engine) Pause(ctx context.Context, itemID int64, reason string) error {
	return e.Transition(ctx, itemID, StatePaused, reason)
}

// Resume returns a paused item to the state it left.
func (e *Engine) Resume(ctx context.Context, itemID int64, reason string) error {
	return store.WithTx(ctx, e.db, func(tx *sql.Tx) error {
		it, err := getItem(ctx, tx, itemID)
		if err != nil {
			return err
		}
		if it.State != StatePaused {
			return fmt.Errorf("%w: item %d is %s, not paused", ErrInvalidTransition, itemID, it.State)
		}
		return e.TransitionTx(ctx, tx, itemID, State(it.PausedFromState), reason)
	})
}

// reopen brings a cancelled item back to new (R3: a song removed and re-added reopens its
// earlier request). It is the one move out of a terminal state.
func (e *Engine) reopen(ctx context.Context, tx store.Execer, it Item, reason string) error {
	if it.State != StateCancelled {
		return fmt.Errorf("%w: item %d is %s, only cancelled items reopen", ErrInvalidTransition, it.ID, it.State)
	}
	return writeState(ctx, tx, it, StateNew, reason, e.Now())
}
