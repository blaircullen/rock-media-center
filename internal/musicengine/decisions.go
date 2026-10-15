package musicengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tristenlammi/arrmada/internal/store"
)

// Decision options other than the ones interfaces.go declares. Each decision lists the
// options that apply to it (mus_decisions.options); Answer refuses any other.
const (
	// OptionLossy takes the official YouTube audio as an honestly labelled lossy file (R15).
	OptionLossy = "lossy"
	// OptionWait keeps waiting for a lossless copy: the item searches again later (R15).
	OptionWait = "wait"
	// OptionApprove lets a held swap go ahead (R22); OptionKeep leaves the incumbent.
	OptionApprove = "approve"
	OptionKeep    = "keep"
)

// DecisionWaitRetry is how long a "keep waiting" answer holds the item before it searches
// again, so a song with no lossless copy yet does not come straight back to the list.
const DecisionWaitRetry = 24 * time.Hour

// Errors Answer returns.
var (
	ErrDecisionNotFound = errors.New("musicengine: decision not found")
	ErrDecisionAnswered = errors.New("musicengine: decision already answered")
	ErrInvalidOption    = errors.New("musicengine: option is not one of this decision's choices")
	// ErrNoAnswerHook means the answer needs a part of the engine (lane or placement) that
	// the wiring has not supplied.
	ErrNoAnswerHook = errors.New("musicengine: no handler wired for this answer")
)

// Decision is one row of mus_decisions.
type Decision struct {
	ID          int64    `json:"id"`
	ItemID      int64    `json:"item_id"`
	Kind        string   `json:"kind"`
	CandidateID int64    `json:"candidate_id,omitempty"`
	JournalID   int64    `json:"journal_id,omitempty"`
	Reason      string   `json:"reason"`
	Options     []string `json:"options"`
	Answer      string   `json:"answer,omitempty"`
	AnswerNote  string   `json:"answer_note,omitempty"`
	CreatedAt   int64    `json:"created_at"`
	AnsweredAt  int64    `json:"answered_at,omitempty"`
}

// AnswerHooks are the parts of an answer that belong to the lanes and to placement. Each
// hook is called with the decision and its item (still waiting_blair) after the common
// checks; it must move the item on itself (lossy -> placing the staged lossy candidate,
// approve -> swapping, treat_match -> done or placing from the library file). A nil hook
// makes that answer fail with ErrNoAnswerHook and change nothing.
type AnswerHooks struct {
	TakeLossy    func(ctx context.Context, d Decision, it Item) error
	ApproveSwap  func(ctx context.Context, d Decision, it Item) error
	TreatAsMatch func(ctx context.Context, d Decision, it Item) error
}

// AnswerResult says what an answer did. An ignored answer is closed with its reason and
// changes nothing else (KTD12).
type AnswerResult struct {
	DecisionID int64
	Option     string
	Ignored    bool
	Reason     string
	State      State // the item's state afterwards
}

// Decisions is the decision list (R15, R22, KTD12): it lists open decisions and routes
// each answer to the engine transition it stands for.
type Decisions struct {
	e     *Engine
	hooks AnswerHooks
}

// NewDecisions builds the list over an engine.
func NewDecisions(e *Engine, hooks AnswerHooks) *Decisions {
	return &Decisions{e: e, hooks: hooks}
}

const decCols = `id, item_id, kind, COALESCE(candidate_id, 0), COALESCE(journal_id, 0), reason, options,
	answer, answer_note, created_at, answered_at`

func decScan(r rowScanner) (Decision, error) {
	var d Decision
	var opts string
	if err := r.Scan(&d.ID, &d.ItemID, &d.Kind, &d.CandidateID, &d.JournalID, &d.Reason, &opts,
		&d.Answer, &d.AnswerNote, &d.CreatedAt, &d.AnsweredAt); err != nil {
		return Decision{}, err
	}
	if err := json.Unmarshal([]byte(opts), &d.Options); err != nil {
		return Decision{}, fmt.Errorf("decision %d has unreadable options %q: %w", d.ID, opts, err)
	}
	return d, nil
}

// Get reads one decision.
func (d *Decisions) Get(ctx context.Context, id int64) (Decision, error) {
	return decGet(ctx, d.e.db, id)
}

func decGet(ctx context.Context, q store.Execer, id int64) (Decision, error) {
	dec, err := decScan(q.QueryRowContext(ctx, `SELECT `+decCols+` FROM mus_decisions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Decision{}, fmt.Errorf("%w: %d", ErrDecisionNotFound, id)
	}
	if err != nil {
		return Decision{}, fmt.Errorf("reading decision %d: %w", id, err)
	}
	return dec, nil
}

// Open lists the decisions waiting on Blair, oldest first.
func (d *Decisions) Open(ctx context.Context) ([]Decision, error) {
	rows, err := d.e.db.QueryContext(ctx,
		`SELECT `+decCols+` FROM mus_decisions WHERE answered_at = 0 ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("listing open decisions: %w", err)
	}
	defer rows.Close()
	var out []Decision
	for rows.Next() {
		dec, err := decScan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dec)
	}
	return out, rows.Err()
}

// Answer applies Blair's choice. Before acting it re-reads the item (KTD12): an item that
// is no longer waiting for Blair, or whose song has left the playlist, closes the decision
// as ignored with the reason; a staged candidate that expired starts a new search instead
// of using it. The decision is marked answered in the same transaction as the item's
// move, so a failure leaves it open.
func (d *Decisions) Answer(ctx context.Context, decisionID int64, option, note string) (AnswerResult, error) {
	dec, err := d.Get(ctx, decisionID)
	if err != nil {
		return AnswerResult{}, err
	}
	if dec.AnsweredAt != 0 {
		return AnswerResult{}, fmt.Errorf("%w: %d", ErrDecisionAnswered, decisionID)
	}
	if !decHasOption(dec.Options, option) {
		return AnswerResult{}, fmt.Errorf("%w: %q for decision %d", ErrInvalidOption, option, decisionID)
	}
	res := AnswerResult{DecisionID: dec.ID, Option: option}

	it, err := d.e.GetItem(ctx, dec.ItemID)
	if err != nil {
		return res, err
	}
	if it.State != StateWaitingBlair {
		res.Ignored, res.State = true, it.State
		res.Reason = d.ignoredReason(ctx, it)
		return res, d.closeIgnored(ctx, dec, option, note, res.Reason)
	}

	switch {
	case option == OptionDrop:
		return d.finish(ctx, dec, it, res, note, StateDropped, "dropped by Blair", 0)
	case option == OptionRetry:
		return d.finish(ctx, dec, it, res, note, decRetryTarget(dec, it), "Blair asked to try again", 0)
	case option == OptionNotAMatch:
		return d.finish(ctx, dec, it, res, note, StateSearching, "Blair: the library file is not this recording", 0)
	case option == OptionWait:
		why := "Blair chose to keep waiting for a lossless copy"
		hold := DecisionWaitRetry
		if expired, detail := d.stagedExpired(ctx, dec); expired {
			why, hold = "Blair chose to keep waiting; the staged candidate "+detail+", so searching again", 0
		}
		return d.finish(ctx, dec, it, res, note, StateSearching, why, hold)
	case option == OptionKeep:
		return d.finish(ctx, dec, it, res, note, StateDropped, "Blair kept the current version", 0)
	case strings.HasPrefix(option, OptionUsePrefix):
		mbid := strings.TrimPrefix(option, OptionUsePrefix)
		if err := d.e.PinToRecording(ctx, it.ID, mbid); err != nil {
			return res, err
		}
		cur, err := d.e.GetItem(ctx, it.ID)
		if err != nil {
			return res, err
		}
		res.State = cur.State
		return res, d.mark(ctx, d.e.db, dec.ID, option, note)
	case option == OptionLossy || option == OptionApprove:
		if expired, detail := d.stagedExpired(ctx, dec); expired {
			return d.finish(ctx, dec, it, res, note, StateSearching,
				"Blair answered "+option+" but the staged candidate "+detail+", so searching again", 0)
		}
		hook := d.hooks.TakeLossy
		if option == OptionApprove {
			hook = d.hooks.ApproveSwap
		}
		return d.viaHook(ctx, dec, it, res, option, note, hook)
	case option == OptionTreatAsMatch:
		return d.viaHook(ctx, dec, it, res, option, note, d.hooks.TreatAsMatch)
	}
	return res, fmt.Errorf("%w: %q has no handler", ErrInvalidOption, option)
}

func (d *Decisions) viaHook(ctx context.Context, dec Decision, it Item, res AnswerResult, option, note string,
	hook func(context.Context, Decision, Item) error) (AnswerResult, error) {
	if hook == nil {
		return res, fmt.Errorf("%w: %q (decision %d)", ErrNoAnswerHook, option, dec.ID)
	}
	if err := hook(ctx, dec, it); err != nil {
		return res, fmt.Errorf("applying %q to item %d: %w", option, it.ID, err)
	}
	cur, err := d.e.GetItem(ctx, it.ID)
	if err != nil {
		return res, err
	}
	res.State = cur.State
	return res, d.mark(ctx, d.e.db, dec.ID, option, note)
}

// finish records the answer and moves the item in one transaction. hold, when positive,
// delays the item's next step.
func (d *Decisions) finish(ctx context.Context, dec Decision, it Item, res AnswerResult, note string, to State, why string, hold time.Duration) (AnswerResult, error) {
	err := store.WithTx(ctx, d.e.db, func(tx *sql.Tx) error {
		if err := d.mark(ctx, tx, dec.ID, res.Option, note); err != nil {
			return err
		}
		if err := d.e.TransitionTx(ctx, tx, it.ID, to, why); err != nil {
			return err
		}
		if hold > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE mus_items SET next_attempt_at = ? WHERE id = ?`,
				d.e.Now().Add(hold).Unix(), it.ID); err != nil {
				return fmt.Errorf("holding item %d after its answer: %w", it.ID, err)
			}
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	res.State = to
	return res, nil
}

func (d *Decisions) mark(ctx context.Context, q store.Execer, id int64, option, note string) error {
	r, err := q.ExecContext(ctx,
		`UPDATE mus_decisions SET answer = ?, answer_note = ?, answered_at = ? WHERE id = ? AND answered_at = 0`,
		option, clip(note, 500), d.e.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("recording the answer to decision %d: %w", id, err)
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return fmt.Errorf("%w: %d", ErrDecisionAnswered, id)
	}
	return nil
}

// closeIgnored closes a decision that no longer applies, keeping the reason with the
// option Blair picked.
func (d *Decisions) closeIgnored(ctx context.Context, dec Decision, option, note, reason string) error {
	text := "ignored: " + reason
	if strings.TrimSpace(note) != "" {
		text += " | " + note
	}
	return d.mark(ctx, d.e.db, dec.ID, option, text)
}

// ignoredReason says why an item no longer takes an answer: its song left the playlist, or
// the engine moved it on.
func (d *Decisions) ignoredReason(ctx context.Context, it Item) string {
	var total, present int
	err := d.e.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(present), 0) FROM mus_playlist_entries WHERE item_id = ?`, it.ID).Scan(&total, &present)
	if err != nil {
		d.e.log.Warn("musicengine: could not check whether a song left the playlist", "item", it.ID, "err", err)
	} else if total > 0 && present == 0 {
		return fmt.Sprintf("the song was removed from the Spotify playlist (item is now %s)", it.State)
	}
	return fmt.Sprintf("the item is now %s, not waiting for you", it.State)
}

// stagedExpired reports whether the decision's staged candidate is gone: past its expiry,
// or its files no longer on disk.
func (d *Decisions) stagedExpired(ctx context.Context, dec Decision) (bool, string) {
	if dec.CandidateID == 0 {
		return false, ""
	}
	var expires int64
	var path, state string
	err := d.e.db.QueryRowContext(ctx,
		`SELECT expires_at, staging_path, state FROM mus_candidates WHERE id = ?`, dec.CandidateID).Scan(&expires, &path, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return true, "no longer exists"
	}
	if err != nil {
		d.e.log.Warn("musicengine: could not read the staged candidate, treating it as unusable", "candidate", dec.CandidateID, "err", err)
		return true, "could not be read"
	}
	if expires > 0 && expires <= d.e.Now().Unix() {
		return true, "expired from staging"
	}
	if path != "" {
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return true, "is no longer in staging"
			}
			d.e.log.Warn("musicengine: could not look at the staged candidate", "path", path, "err", err)
		}
	}
	return false, ""
}

func decHasOption(opts []string, o string) bool {
	for _, x := range opts {
		if x == o {
			return true
		}
	}
	return false
}

// decRetryTarget is the state a "try again" answer sends the item back to: the step that
// raised the decision.
func decRetryTarget(dec Decision, it Item) State {
	switch dec.Kind {
	case "ambiguous":
		if it.RecordingID == 0 {
			return StatePinning
		}
		return StateLibraryCheck
	case "blind_library":
		return StateLibraryCheck
	}
	return StateSearching
}
