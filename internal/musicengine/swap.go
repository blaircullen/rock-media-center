package musicengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tristenlammi/arrmada/internal/audiocheck"
	"github.com/tristenlammi/arrmada/internal/plex"
	"github.com/tristenlammi/arrmada/internal/store"
)

// Swaps is the engine's swap Placer (U11). Placement.Step forwards the swapping and
// waiting_for_playback states here (PlacementConfig.Swaps), and calls Finalize for swap and
// undo rows before it records plex_confirmed (PlacementConfig.Finalize).
//
// A swap, in order:
//
//  1. The library file is rechecked. A file the checker cannot read is retried, never
//     replaced (R14, AE6).
//  2. compare.go decides: swap, keep, or ask Blair (R21, R22, KTD19). With automatic swaps
//     off (KTD16) a swap becomes a question too.
//  3. If Plex reports the track playing, the item waits (waiting_for_playback) for up to
//     PlaybackWait and then goes back to the next sweep cycle (R24).
//  4. The track's rating and playlist memberships are recorded (KTD8), then U10's journal
//     runs the phases: stage, quarantine the incumbent, publish over it after proving the
//     destination still holds that incumbent (KTD18).
//  5. Finalize re-applies rating and memberships and reads them back from Plex.
//
// Wiring (U15), because Swaps and Placement refer to each other:
//
//	sw, _ := NewSwaps(SwapsConfig{...})
//	pl, _ := NewPlacement(PlacementConfig{..., Swaps: sw, Finalize: sw.Finalize})
//	pl.Bind(engine)
//	sw.Bind(pl)
type SwapPlex interface {
	PlexWriter
	Sessions(ctx context.Context) ([]plex.Session, error)
}

var (
	_ SwapPlex = (*plex.Client)(nil)
	_ Placer   = (*Swaps)(nil)
)

// Defaults.
const (
	// DefaultPlaybackWait is how long a swap waits for a playing track before it gives the
	// track back to the next sweep cycle (R24).
	DefaultPlaybackWait = DeadlineWaitingForPlayback
	swapPlaybackPoll    = time.Minute
	// swapApprovedPrefix marks the reason of an item Blair's approval moved back to swapping.
	swapApprovedPrefix = "swap approved by Blair, decision "
)

// SwapsConfig wires Swaps.
type SwapsConfig struct {
	Checker    AudioChecker
	Plex       SwapPlex
	Quarantine *Quarantine
	// Decisions raises the questions; nil means the table writer.
	Decisions DecisionRaiser
	// AutoSwaps is the automatic-swaps setting (KTD16), read fresh on each use. Nil means off.
	AutoSwaps func() bool
	// PlaybackWait bounds the wait for a playing track; 0 means DefaultPlaybackWait.
	PlaybackWait time.Duration
	// SweepBase and SweepMax are the R26 retry interval bounds; 0 means the defaults.
	SweepBase, SweepMax time.Duration
	Now                 func() time.Time
	Log                 *slog.Logger
}

// Swaps implements Placer for swapping and waiting_for_playback.
type Swaps struct {
	cfg SwapsConfig
	p   *Placement
	log *slog.Logger
}

// NewSwaps validates the config.
func NewSwaps(cfg SwapsConfig) (*Swaps, error) {
	if cfg.Checker == nil || cfg.Plex == nil || cfg.Quarantine == nil {
		return nil, errors.New("musicengine swaps: a checker, a Plex client and a quarantine are required")
	}
	if cfg.Decisions == nil {
		cfg.Decisions = tableDecisions{}
	}
	if cfg.PlaybackWait <= 0 {
		cfg.PlaybackWait = DefaultPlaybackWait
	}
	if cfg.SweepBase <= 0 {
		cfg.SweepBase = DefaultSweepBase
	}
	if cfg.SweepMax <= 0 {
		cfg.SweepMax = DefaultSweepMax
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Swaps{cfg: cfg, log: log}, nil
}

// Bind gives Swaps the Placement it drives. Call it after pl.Bind(engine).
func (s *Swaps) Bind(p *Placement) error {
	if p == nil {
		return errors.New("musicengine swaps: no placement")
	}
	if p.cfg.QuarantineRoot == "" || p.cfg.QuarantineRoot != s.cfg.Quarantine.Root() {
		return fmt.Errorf("musicengine swaps: placement quarantine root %q differs from the quarantine's %q", p.cfg.QuarantineRoot, s.cfg.Quarantine.Root())
	}
	s.p = p
	p.playing = s.rowPlaying
	return nil
}

// rowPlaying is Placement's last-look hook: whether Plex reports the row's track playing now.
// A track the engine cannot find is an error (the replacement stays pending), never "idle".
func (s *Swaps) rowPlaying(ctx context.Context, row JournalRow) (bool, error) {
	if row.TrackID == 0 {
		return false, fmt.Errorf("journal row %d has no library track, so playback cannot be checked", row.ID)
	}
	track, err := swapLoadTrack(ctx, s.p.m.DB(), row.TrackID)
	if err != nil {
		return false, err
	}
	return s.isPlaying(ctx, track.RatingKey)
}

func (s *Swaps) autoSwaps() bool { return s.cfg.AutoSwaps != nil && s.cfg.AutoSwaps() }

func (s *Swaps) machine() (Machine, error) {
	if s.p == nil || s.p.m == nil || s.p.jr == nil {
		return nil, errors.New("musicengine swaps: not bound to a placement and machine")
	}
	return s.p.m, nil
}

// ---- rows ----

type swapTrack struct {
	ID          int64
	RatingKey   string
	Path        string // as Plex sees it
	Title       string
	Artist      string
	Album       string
	ReleaseGrp  string
	Explicit    bool
	RecordingID int64
}

func swapLoadTrack(ctx context.Context, q store.Execer, id int64) (swapTrack, error) {
	var t swapTrack
	var explicit int
	var rec sql.NullInt64
	err := q.QueryRowContext(ctx,
		`SELECT id, plex_rating_key, path, title, artist, album, release_group_mbid, explicit, recording_id
		 FROM mus_library_tracks WHERE id = ?`, id).
		Scan(&t.ID, &t.RatingKey, &t.Path, &t.Title, &t.Artist, &t.Album, &t.ReleaseGrp, &explicit, &rec)
	if errors.Is(err, sql.ErrNoRows) {
		return t, fmt.Errorf("library track %d not found", id)
	}
	if err != nil {
		return t, fmt.Errorf("reading library track %d: %w", id, err)
	}
	t.Explicit, t.RecordingID = explicit != 0, rec.Int64
	return t, nil
}

type swapCand struct {
	ID           int64
	Lane         string
	ReleaseName  string
	StagingPath  string
	SHA256       string
	Fingerprint  string
	Verdict      string
	Codec        string
	BitDepth     int
	SampleRate   int
	Bitrate      int
	DurationMs   int64
	DecodedMs    int64
	Loudness     sql.NullFloat64
	TopHz        int
	ReleaseGroup string
	IntegrityOK  bool
}

func swapChosen(ctx context.Context, q store.Execer, itemID int64) (swapCand, bool, error) {
	var c swapCand
	var ok int
	err := q.QueryRowContext(ctx,
		`SELECT id, lane, release_name, staging_path, sha256, pcm_fingerprint, verdict, codec, bit_depth, sample_rate,
		        bitrate, duration_ms, decoded_ms, loudness_lufs, spectral_top_hz, release_group_mbid, integrity_ok
		 FROM mus_candidates WHERE item_id = ? AND state = 'chosen' ORDER BY id DESC LIMIT 1`, itemID).
		Scan(&c.ID, &c.Lane, &c.ReleaseName, &c.StagingPath, &c.SHA256, &c.Fingerprint, &c.Verdict, &c.Codec, &c.BitDepth,
			&c.SampleRate, &c.Bitrate, &c.DurationMs, &c.DecodedMs, &c.Loudness, &c.TopHz, &c.ReleaseGroup, &ok)
	if errors.Is(err, sql.ErrNoRows) {
		return swapCand{}, false, nil
	}
	if err != nil {
		return swapCand{}, false, fmt.Errorf("reading the chosen candidate of item %d: %w", itemID, err)
	}
	c.IntegrityOK = ok != 0
	return c, true, nil
}

// swapExplicit reads whether a file is the explicit version: words in its name win, then the
// engine's own flag on the track, then the pinned recording's flag.
func swapExplicit(text string, trackFlag, recordingFlag bool) bool {
	switch cmpExplicitHint(text) {
	case 1:
		return true
	case -1:
		return false
	}
	return trackFlag || recordingFlag
}

func swapLengthMs(decoded, probed int64) int64 {
	if decoded > 0 {
		return decoded
	}
	return probed
}

func swapNullFloat(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
}

// swapBlocked reports whether a candidate is blocked for the recording: by an undo (the
// blocked table) or by Blair keeping the current file against it (a "keep" answer).
func swapBlocked(ctx context.Context, q store.Execer, recordingID int64, fingerprint string) (bool, error) {
	if fingerprint == "" || recordingID == 0 {
		return false, nil
	}
	var n int
	err := q.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM mus_blocked_candidates WHERE recording_id = ? AND pcm_fingerprint = ?)
		      + (SELECT COUNT(*) FROM mus_decisions d
		           JOIN mus_candidates c ON c.id = d.candidate_id
		           JOIN mus_items i ON i.id = d.item_id
		          WHERE d.answer = ? AND d.kind IN (?, ?, ?, ?) AND i.recording_id = ? AND c.pcm_fingerprint = ?)`,
		recordingID, fingerprint, OptionKeep,
		DecisionDifferentMaster, DecisionTradeOff, DecisionPathChange, DecisionSwapDisabled,
		recordingID, fingerprint).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("checking blocked candidates of recording %d: %w", recordingID, err)
	}
	return n > 0, nil
}

// swapApproved reports whether Blair said yes to this candidate.
func swapApproved(ctx context.Context, q store.Execer, it Item, candID int64) (bool, error) {
	if strings.HasPrefix(it.LastReason, swapApprovedPrefix) {
		return true, nil
	}
	var n int
	err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mus_decisions WHERE item_id = ? AND candidate_id = ? AND answer = ? AND answered_at > 0`,
		it.ID, candID, OptionApprove).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("checking approvals for item %d: %w", it.ID, err)
	}
	return n > 0, nil
}

// ---- Step ----

// Step is the Placer. It is safe to call twice: an open journal row says where a swap was.
func (s *Swaps) Step(ctx context.Context, m Machine, it Item) error {
	if s.p == nil {
		return errors.New("musicengine swaps: not bound to a placement")
	}
	cur, err := getItem(ctx, m.DB(), it.ID)
	if err != nil {
		return err
	}
	switch cur.State {
	case StateWaitingForPlayback:
		return s.stepWaiting(ctx, m, cur)
	case StateSwapping:
		return s.stepSwapping(ctx, m, cur)
	}
	return nil
}

// isPlaying asks Plex whether the track is being played right now. A Plex that cannot answer
// is an error, never "not playing".
func (s *Swaps) isPlaying(ctx context.Context, ratingKey string) (bool, error) {
	if ratingKey == "" {
		return false, nil
	}
	ss, err := s.cfg.Plex.Sessions(ctx)
	if err != nil {
		return false, fmt.Errorf("asking Plex who is playing: %w", err)
	}
	for _, x := range ss {
		if x.RatingKey == ratingKey {
			return true, nil
		}
	}
	return false, nil
}

func (s *Swaps) stepWaiting(ctx context.Context, m Machine, it Item) error {
	track, err := swapLoadTrack(ctx, m.DB(), it.TrackID)
	if err != nil {
		return err
	}
	playing, err := s.isPlaying(ctx, track.RatingKey)
	if err != nil {
		return err
	}
	if !playing {
		return m.Transition(ctx, it.ID, StateSwapping, "playback stopped; swapping now")
	}
	waited := m.Now().Sub(time.Unix(it.EnteredAt, 0))
	if waited < s.cfg.PlaybackWait {
		_, err := m.DB().ExecContext(ctx, `UPDATE mus_items SET next_attempt_at = ?, updated_at = ? WHERE id = ? AND state = ?`,
			m.Now().Add(swapPlaybackPoll).Unix(), m.Now().Unix(), it.ID, string(StateWaitingForPlayback))
		return err
	}
	return s.deferSwap(ctx, m, it, track, fmt.Sprintf("the track kept playing for %s; deferred to the next sweep cycle", waited.Round(time.Minute)))
}

// deferSwap gives the track back to the sweep: any staged temp file is removed, the item ends
// and the track is revisited soon without doubling its retry interval.
func (s *Swaps) deferSwap(ctx context.Context, m Machine, it Item, track swapTrack, reason string) error {
	if row, ok, err := s.p.jr.OpenForItem(ctx, it.ID); err != nil {
		return err
	} else if ok {
		if err := s.rollbackUnpublished(ctx, row, "deferred: "+reason); err != nil {
			return err
		}
	}
	return store.WithTx(ctx, m.DB(), func(tx *sql.Tx) error {
		if err := swapFinishItem(ctx, tx, it.ID, StateDropped, reason, m.Now()); err != nil {
			return err
		}
		return sweepDeferTrack(ctx, tx, track.ID, m.Now(), sweepDeferAfter)
	})
}

// rollbackUnpublished rolls a row back under the destination's lock, and only if it is still
// open and has not published: another path may have advanced it since the caller read it.
func (s *Swaps) rollbackUnpublished(ctx context.Context, row JournalRow, reason string) error {
	unlock, err := s.p.lock(ctx, row.NewPath)
	if err != nil {
		return err
	}
	defer unlock()
	cur, err := s.p.jr.Get(ctx, row.ID)
	if err != nil {
		return err
	}
	if !cur.Open() || cur.Phase >= PhasePublished {
		return nil
	}
	_, err = s.p.rollback(ctx, cur, reason)
	return err
}

// swapFinishItem ends an item that will not swap, inside the caller's transaction. It leaves
// an already-terminal item alone and refuses any move the transition table does not allow.
func swapFinishItem(ctx context.Context, q store.Execer, itemID int64, to State, reason string, now time.Time) error {
	it, err := getItem(ctx, q, itemID)
	if err != nil {
		return err
	}
	if it.State.Terminal() {
		return nil
	}
	if !allowed(it.State, to, State(it.PausedFromState)) {
		return fmt.Errorf("%w: item %d %s -> %s", ErrInvalidTransition, itemID, it.State, to)
	}
	return writeState(ctx, q, it, to, clip(reason, 300), now)
}

func (s *Swaps) stepSwapping(ctx context.Context, m Machine, it Item) error {
	if it.TrackID == 0 {
		return fmt.Errorf("swapping item %d has no library track", it.ID)
	}
	if it.RecordingID == 0 {
		return fmt.Errorf("swapping item %d has no pinned recording", it.ID)
	}
	track, err := swapLoadTrack(ctx, m.DB(), it.TrackID)
	if err != nil {
		return err
	}
	if row, ok, err := s.p.jr.OpenForItem(ctx, it.ID); err != nil {
		return err
	} else if ok {
		if row.Kind != JournalSwap {
			return fmt.Errorf("item %d is swapping but its open journal row %d is a %s", it.ID, row.ID, row.Kind)
		}
		return s.advanceRow(ctx, m, it, track, row)
	}
	return s.startSwap(ctx, m, it, track)
}

// advanceRow takes an open row forward. A row that rolled back (it never got past staging)
// starts again from the top.
func (s *Swaps) advanceRow(ctx context.Context, m Machine, it Item, track swapTrack, row JournalRow) error {
	if row.Phase < PhasePublished {
		playing, err := s.isPlaying(ctx, track.RatingKey)
		if err != nil {
			return err
		}
		if playing {
			return s.waitForPlayback(ctx, m, it)
		}
	}
	out, err := s.p.Advance(ctx, row)
	if err != nil {
		return s.p.waitIfNoSpace(ctx, it, err)
	}
	switch out {
	case OutcomeRolledBack:
		return s.startSwap(ctx, m, it, track)
	case OutcomePending:
		return s.waitForPlayback(ctx, m, it) // it started playing after the checks above
	}
	return nil // advanced (the item now awaits Plex) or stopped for Blair
}

func (s *Swaps) waitForPlayback(ctx context.Context, m Machine, it Item) error {
	return store.WithTx(ctx, m.DB(), func(tx *sql.Tx) error {
		if err := m.TransitionTx(ctx, tx, it.ID, StateWaitingForPlayback, "the track is playing; waiting for it to stop"); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE mus_items SET next_attempt_at = ? WHERE id = ?`, m.Now().Add(swapPlaybackPoll).Unix(), it.ID)
		return err
	})
}

func (s *Swaps) startSwap(ctx context.Context, m Machine, it Item, track swapTrack) error {
	cand, ok, err := swapChosen(ctx, m.DB(), it.ID)
	if err != nil {
		return err
	}
	if !ok {
		return m.Transition(ctx, it.ID, StateSearching, "swap found no chosen candidate")
	}
	if err := s.p.layout.Check(); err != nil {
		return err // storage unreachable: the item stays here and nothing is touched
	}
	local := s.p.paths.toLocal(track.Path)
	if err := s.p.layout.Inside(local); err != nil {
		return err
	}
	if fi, err := os.Stat(cand.StagingPath); err != nil || !fi.Mode().IsRegular() {
		return s.rejectCandidate(ctx, m, it, cand, fmt.Sprintf("the staged file is gone: %v", err))
	}
	if fi, err := os.Stat(local); err != nil || !fi.Mode().IsRegular() {
		return store.WithTx(ctx, m.DB(), func(tx *sql.Tx) error {
			if err := swapFinishItem(ctx, tx, it.ID, StateDropped, fmt.Sprintf("the library file %s is missing (%v); the next catalogue refresh will correct the track", local, err), m.Now()); err != nil {
				return err
			}
			return sweepMissTrack(ctx, tx, track.ID, m.Now(), s.cfg.SweepBase, s.cfg.SweepMax)
		})
	}

	playing, err := s.isPlaying(ctx, track.RatingKey)
	if err != nil {
		return err
	}
	if playing {
		return s.waitForPlayback(ctx, m, it)
	}

	// Recheck the incumbent: an unreadable file is retried, never swapped out (AE6, R14).
	res := s.cfg.Checker.Check(ctx, local)
	if res.Verdict == audiocheck.Blind {
		if it.Attempts+1 >= maxBlindAttempts {
			return store.WithTx(ctx, m.DB(), func(tx *sql.Tx) error {
				reason := fmt.Sprintf("the library file %s could not be read %d times (%s); it was not replaced", local, maxBlindAttempts, res.Reason)
				if err := s.cfg.Decisions.Raise(ctx, tx, DecisionRequest{
					ItemID: it.ID, Kind: DecisionBlindIncumbent, Reason: reason,
					Options: []string{OptionRetry, OptionDrop}, Now: m.Now(),
				}); err != nil {
					return err
				}
				return m.TransitionTx(ctx, tx, it.ID, StateWaitingBlair, reason)
			})
		}
		return fmt.Errorf("the library file %s could not be read, so it was not compared (will retry): %s", local, res.Reason)
	}
	incSHA, err := placeSHA256(local)
	if err != nil {
		return fmt.Errorf("checksumming the library file %s: %w", local, err)
	}
	now := m.Now()
	if err := store.WithTx(ctx, m.DB(), func(tx *sql.Tx) error {
		if _, err := persistCheck(ctx, tx, libCand{dbID: track.ID, path: track.Path, ratingKey: track.RatingKey}, it.RecordingID, res, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE mus_library_tracks SET sha256 = ? WHERE id = ?`, incSHA, track.ID)
		return err
	}); err != nil {
		return err
	}

	rec, err := getRecording(ctx, m.DB(), it.RecordingID)
	if err != nil {
		return err
	}
	blocked, err := swapBlocked(ctx, m.DB(), it.RecordingID, cand.Fingerprint)
	if err != nil {
		return err
	}
	approved, err := swapApproved(ctx, m.DB(), it, cand.ID)
	if err != nil {
		return err
	}
	verdict := cmpDecide(CmpInput{
		Incumbent: swapIncumbentFacts(local, track, rec, res, incSHA),
		Candidate: swapCandidateFacts(cand, rec),
		Blocked:   blocked, AutoSwaps: s.autoSwaps(), Approved: approved,
	})
	s.log.Info("musicengine swap comparison", "item", it.ID, "track", track.ID, "candidate", cand.ID, "action", verdict.Action.String(), "kind", verdict.Kind, "reason", verdict.Reason)
	switch verdict.Action {
	case CmpKeep:
		if verdict.Blind {
			return fmt.Errorf("the library file %s could not be compared: %s", local, verdict.Reason)
		}
		return s.keepIncumbent(ctx, m, it, track, cand, verdict.Reason)
	case CmpAsk:
		return s.askBlair(ctx, m, it, track, cand, verdict)
	}
	return s.runSwap(ctx, m, it, track, cand, local, incSHA, res, verdict.Reason)
}

func swapIncumbentFacts(local string, t swapTrack, rec Recording, res audiocheck.Result, sha string) CmpFacts {
	f := CmpFacts{
		Path: local, Codec: res.Facts.Codec, BitDepth: res.Facts.BitDepth, SampleRate: res.Facts.SampleRate,
		BitrateKbps: int(res.Facts.BitrateKbps + 0.5), Verdict: string(res.Verdict), Fingerprint: res.Fingerprint,
		LengthMs:     swapLengthMs(int64(res.Integrity.DecodedSec*1000), int64(res.Facts.DurationSec*1000)),
		LoudnessLUFS: res.LoudnessLUFS, ReleaseGroupMBID: t.ReleaseGrp,
		Explicit: swapExplicit(local+" "+t.Album+" "+t.Title, t.Explicit, rec.Explicit),
	}
	if res.Signature != nil {
		f.TopHz = int(res.Signature.TopHz)
	}
	return f
}

func swapCandidateFacts(c swapCand, rec Recording) CmpFacts {
	return CmpFacts{
		Path: c.StagingPath, Codec: c.Codec, BitDepth: c.BitDepth, SampleRate: c.SampleRate, BitrateKbps: c.Bitrate,
		Verdict: c.Verdict, Fingerprint: c.Fingerprint, TopHz: c.TopHz,
		LengthMs: swapLengthMs(c.DecodedMs, c.DurationMs), LoudnessLUFS: swapNullFloat(c.Loudness),
		ReleaseGroupMBID: c.ReleaseGroup,
		Explicit:         swapExplicit(c.ReleaseName, false, rec.Explicit),
	}
}

// rejectCandidate marks the candidate unusable and sends the item back to searching.
func (s *Swaps) rejectCandidate(ctx context.Context, m Machine, it Item, c swapCand, reason string) error {
	return store.WithTx(ctx, m.DB(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE mus_candidates SET state = 'rejected', reject_reason = ? WHERE id = ?`, clip(reason, 500), c.ID); err != nil {
			return fmt.Errorf("rejecting candidate %d: %w", c.ID, err)
		}
		return m.TransitionTx(ctx, tx, it.ID, StateSearching, "candidate failed the swap: "+clip(reason, 200))
	})
}

// keepIncumbent records that this candidate is not an upgrade. If the item has other found
// candidates it goes back to searching to try the next; otherwise a sweep visit ends as a
// miss and its track is retried on a doubled interval (R26).
func (s *Swaps) keepIncumbent(ctx context.Context, m Machine, it Item, track swapTrack, c swapCand, reason string) error {
	return store.WithTx(ctx, m.DB(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE mus_candidates SET state = 'rejected', reject_reason = ? WHERE id = ?`, clip("not an upgrade: "+reason, 500), c.ID); err != nil {
			return fmt.Errorf("rejecting candidate %d: %w", c.ID, err)
		}
		var more int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mus_candidates WHERE item_id = ? AND state = 'found'`, it.ID).Scan(&more); err != nil {
			return fmt.Errorf("counting remaining candidates of item %d: %w", it.ID, err)
		}
		if more > 0 || it.Kind != KindSweep {
			return m.TransitionTx(ctx, tx, it.ID, StateSearching, "kept the library file: "+clip(reason, 200)+"; trying the next candidate")
		}
		if err := swapFinishItem(ctx, tx, it.ID, StateDropped, "kept the library file: "+reason, m.Now()); err != nil {
			return err
		}
		return sweepMissTrack(ctx, tx, track.ID, m.Now(), s.cfg.SweepBase, s.cfg.SweepMax)
	})
}

// askBlair puts the swap on the decision list (R22, KTD16). The staged candidate stays
// where it is; the item waits and holds the recording against a second visit.
func (s *Swaps) askBlair(ctx context.Context, m Machine, it Item, track swapTrack, c swapCand, v CmpResult) error {
	options := []string{OptionApprove, OptionKeep, OptionDrop}
	reason := v.Reason
	if v.Kind == DecisionPathChange {
		// The journal replaces a file in place; it cannot move a track to a new file name.
		options = []string{OptionKeep, OptionDrop}
		reason += " (the engine cannot rename a library track; replace it by hand, or keep the current file)"
	}
	return store.WithTx(ctx, m.DB(), func(tx *sql.Tx) error {
		if err := s.cfg.Decisions.Raise(ctx, tx, DecisionRequest{
			ItemID: it.ID, Kind: v.Kind, Reason: reason, Options: options, Now: m.Now(),
		}); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE mus_decisions SET candidate_id = ? WHERE item_id = ? AND kind = ? AND answered_at = 0`, c.ID, it.ID, v.Kind); err != nil {
			return fmt.Errorf("tying the %s decision of item %d to candidate %d: %w", v.Kind, it.ID, c.ID, err)
		}
		if it.Kind == KindSweep {
			if err := sweepMissTrack(ctx, tx, track.ID, m.Now(), s.cfg.SweepBase, s.cfg.SweepMax); err != nil {
				return err
			}
		}
		return m.TransitionTx(ctx, tx, it.ID, StateWaitingBlair, clip(reason, 300))
	})
}

// ---- the swap itself ----

type swapMember struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	Mirror bool   `json:"mirror"`
}

func swapEncodeMembers(ms []swapMember) (string, error) {
	if len(ms) == 0 {
		return "", nil
	}
	b, err := json.Marshal(ms)
	if err != nil {
		return "", fmt.Errorf("encoding memberships: %w", err)
	}
	return string(b), nil
}

// swapSnapshot is what Plex holds about a track before a change (KTD8).
type swapSnapshot struct {
	Rating  *float64
	Members []swapMember
}

// snapshot reads the track's rating and playlist memberships live from Plex. A read that
// fails stops the swap: replacing a file without knowing what to restore is the thing the
// plan forbids.
func (s *Swaps) snapshot(ctx context.Context, track swapTrack) (swapSnapshot, error) {
	var snap swapSnapshot
	section := s.p.cfg.PlexSection()
	if section == "" {
		return snap, errors.New("no Plex music section is configured")
	}
	found, rating, err := s.plexTrack(ctx, section, track.Artist, track.Title, track.RatingKey)
	if err != nil {
		return snap, err
	}
	if !found {
		return snap, fmt.Errorf("Plex does not list track %s (%s - %s), so its rating cannot be recorded", track.RatingKey, track.Artist, track.Title)
	}
	if rating > 0 {
		r := rating
		snap.Rating = &r
	}
	pls, err := s.cfg.Plex.Playlists(ctx)
	if err != nil {
		return snap, fmt.Errorf("reading Plex playlists: %w", err)
	}
	for _, p := range pls {
		if p.Smart {
			continue
		}
		items, err := s.cfg.Plex.PlaylistItems(ctx, p.RatingKey)
		if err != nil {
			return snap, fmt.Errorf("reading Plex playlist %q: %w", p.Title, err)
		}
		if remapHasKey(items, track.RatingKey) {
			snap.Members = append(snap.Members, swapMember{Key: p.RatingKey, Title: p.Title, Mirror: strings.HasSuffix(p.Title, MirrorSuffix)})
		}
	}
	return snap, nil
}

// plexTrack finds one track in Plex by artist and title and returns its rating.
func (s *Swaps) plexTrack(ctx context.Context, section, artist, title, ratingKey string) (bool, float64, error) {
	ts, err := s.cfg.Plex.SearchTracks(ctx, section, artist, title)
	if err != nil {
		return false, 0, fmt.Errorf("searching Plex for %q: %w", title, err)
	}
	for _, t := range ts {
		if t.RatingKey == ratingKey {
			return true, t.UserRating, nil
		}
	}
	return false, 0, nil
}

// record writes the snapshot to the engine's durable identity tables (R19), so a re-import
// after the swap can restore it too.
func (s *Swaps) record(ctx context.Context, m Machine, recordingID int64, track swapTrack, sha string, snap swapSnapshot) error {
	return store.WithTx(ctx, m.DB(), func(tx *sql.Tx) error {
		id, err := RecordIdentity(ctx, tx, IdentityRecord{
			RecordingID: recordingID, Path: track.Path, SHA256: sha, RatingKey: track.RatingKey, Rating: snap.Rating,
		}, m.Now())
		if err != nil {
			return err
		}
		for _, mem := range snap.Members {
			if err := RecordMembership(ctx, tx, id, mem.Key, mem.Title, mem.Mirror, m.Now()); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Swaps) runSwap(ctx context.Context, m Machine, it Item, track swapTrack, c swapCand, local, incSHA string, inc audiocheck.Result, why string) error {
	snap, err := s.snapshot(ctx, track)
	if err != nil {
		return err
	}
	if err := s.record(ctx, m, it.RecordingID, track, incSHA, snap); err != nil {
		return err
	}
	members, err := swapEncodeMembers(snap.Members)
	if err != nil {
		return err
	}
	var prev int64
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COALESCE(MAX(id), 0) FROM mus_swap_journal WHERE kind = ? AND phase = ? AND (track_id = ? OR new_path = ?)`,
		JournalSwap, PhasePlexConfirmed, track.ID, local).Scan(&prev); err != nil {
		return fmt.Errorf("finding the previous swap of track %d: %w", track.ID, err)
	}
	row, err := s.p.Stage(ctx, StageSpec{
		Kind: JournalSwap, ItemID: it.ID, TrackID: track.ID, CandidateID: c.ID, RecordingID: it.RecordingID,
		Source: c.StagingPath, SourceSHA: c.SHA256, Dest: local, IncumbentSHA: incSHA,
		QuarantinePath:    s.cfg.Quarantine.PathFor(track.ID, "swap-"+strconv.FormatInt(it.ID, 10), incSHA, local),
		ExpectFingerprint: c.Fingerprint, OldFingerprint: inc.Fingerprint,
		RatingBefore: snap.Rating, MembershipsBefore: members, PreviousSwapID: prev,
	})
	if err != nil {
		var ve *VerifyError
		if errors.As(err, &ve) {
			return s.rejectCandidate(ctx, m, it, c, ve.Reason)
		}
		return s.p.waitIfNoSpace(ctx, it, err)
	}
	s.log.Info("musicengine swap staged", "item", it.ID, "row", row.ID, "why", why)
	// Staging takes a while; look at playback again before the file is touched.
	playing, err := s.isPlaying(ctx, track.RatingKey)
	if err != nil {
		return err
	}
	if playing {
		return s.waitForPlayback(ctx, m, it)
	}
	out, err := s.p.Advance(ctx, row)
	if err != nil {
		return s.p.waitIfNoSpace(ctx, it, err)
	}
	if out == OutcomePending {
		return s.waitForPlayback(ctx, m, it)
	}
	return nil
}

// ---- Finalize ----

// Finalize is PlacementConfig.Finalize. For a swap or undo row it re-applies the rating and
// playlist memberships recorded before the change and reads them back from Plex (R24, KTD8);
// any difference is an error, so the row stays unconfirmed and is retried.
func (s *Swaps) Finalize(ctx context.Context, row JournalRow, ratingKey string) error {
	if s.p == nil {
		return errors.New("musicengine swaps: not bound to a placement")
	}
	section := s.p.cfg.PlexSection()
	tags, err := s.p.readTags(ctx, row.NewPath)
	if err != nil {
		return fmt.Errorf("reading tags of %s: %w", row.NewPath, err)
	}
	artist := tags.folderArtist()

	// Rating: put it back only if the swap lost it. A rating Blair changed in the meantime wins.
	found, cur, err := s.plexTrack(ctx, section, artist, tags.Title, ratingKey)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("Plex no longer lists track %s after the swap", ratingKey)
	}
	if row.RatingBefore != nil && *row.RatingBefore > 0 && cur == 0 {
		if err := s.cfg.Plex.SetRating(ctx, ratingKey, *row.RatingBefore); err != nil {
			return fmt.Errorf("re-applying rating %v: %w", *row.RatingBefore, err)
		}
		found, cur, err = s.plexTrack(ctx, section, artist, tags.Title, ratingKey)
		if err != nil {
			return err
		}
		if !found || cur != *row.RatingBefore {
			return fmt.Errorf("rating of %s did not read back as %v (Plex says %v)", ratingKey, *row.RatingBefore, cur)
		}
	}

	// Memberships.
	var members []swapMember
	if strings.TrimSpace(row.MembershipsBefore) != "" {
		if err := json.Unmarshal([]byte(row.MembershipsBefore), &members); err != nil {
			return fmt.Errorf("journal row %d has unreadable memberships %q: %w", row.ID, row.MembershipsBefore, err)
		}
	}
	if len(members) > 0 {
		pls, err := s.cfg.Plex.Playlists(ctx)
		if err != nil {
			return fmt.Errorf("reading Plex playlists: %w", err)
		}
		for _, mem := range members {
			pl, why := remapPlaylistFor(pls, membershipRow{PlaylistKey: mem.Key, Title: mem.Title})
			if why != "" {
				// The playlist is gone; the track cannot be in it. Say so, do not fail the swap.
				if nerr := s.p.jr.Note(ctx, row.ID, why); nerr != nil {
					s.log.Warn("musicengine swap: could not note a missing playlist", "row", row.ID, "err", nerr)
				}
				continue
			}
			items, err := s.cfg.Plex.PlaylistItems(ctx, pl.RatingKey)
			if err != nil {
				return fmt.Errorf("reading Plex playlist %q: %w", pl.Title, err)
			}
			if !remapHasKey(items, ratingKey) {
				if err := s.cfg.Plex.AddToPlaylist(ctx, pl.RatingKey, []string{ratingKey}); err != nil {
					return fmt.Errorf("re-adding the track to playlist %q: %w", pl.Title, err)
				}
				if items, err = s.cfg.Plex.PlaylistItems(ctx, pl.RatingKey); err != nil {
					return fmt.Errorf("reading back Plex playlist %q: %w", pl.Title, err)
				}
				if !remapHasKey(items, ratingKey) {
					return fmt.Errorf("track %s did not read back in playlist %q", ratingKey, pl.Title)
				}
			}
		}
	}
	if row.Kind == JournalSwap && row.TrackID != 0 {
		// The swap worked: this track starts its sweep schedule again.
		if err := sweepResetTrack(ctx, s.p.m.DB(), row.TrackID, s.p.m.Now(), s.cfg.SweepBase); err != nil {
			s.log.Warn("musicengine swap: could not reset the track's sweep schedule", "track", row.TrackID, "err", err)
		}
	}
	return nil
}

// ---- answers ----

// ApproveSwap is AnswerHooks.ApproveSwap: Blair said yes to a held swap. The item goes back to
// swapping with the approval in its reason, and the next step swaps (the safety rules in
// compare.go still apply). Not valid for a path_change question: the engine cannot rename.
func (s *Swaps) ApproveSwap(ctx context.Context, d Decision, it Item) error {
	m, err := s.machine()
	if err != nil {
		return err
	}
	switch d.Kind {
	case DecisionDifferentMaster, DecisionTradeOff, DecisionSwapDisabled:
	case DecisionPathChange:
		return errors.New("a path change cannot be approved: the engine cannot rename a library track")
	default:
		return fmt.Errorf("decision %d is a %q question, not a held swap", d.ID, d.Kind)
	}
	if d.CandidateID == 0 {
		return fmt.Errorf("decision %d names no candidate", d.ID)
	}
	return store.WithTx(ctx, m.DB(), func(tx *sql.Tx) error {
		cur, err := getItem(ctx, tx, it.ID)
		if err != nil {
			return err
		}
		if cur.State != StateWaitingBlair {
			return fmt.Errorf("%w: item %d is %s, not waiting for Blair", ErrInvalidTransition, it.ID, cur.State)
		}
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM mus_candidates WHERE id = ?`, d.CandidateID).Scan(&state); err != nil {
			return fmt.Errorf("reading candidate %d: %w", d.CandidateID, err)
		}
		if state != "chosen" {
			return fmt.Errorf("candidate %d is %s, not the chosen one", d.CandidateID, state)
		}
		return m.TransitionTx(ctx, tx, it.ID, StateSwapping, swapApprovedPrefix+strconv.FormatInt(d.ID, 10))
	})
}

// KeepSwap blocks the candidate of a held swap when Blair keeps the current file. The engine
// already treats a "keep" answer as a block (swapBlocked reads it, and Sweep.Run writes it to
// the blocked table), so wiring this is optional; it makes the block immediate.
func (s *Swaps) KeepSwap(ctx context.Context, d Decision, it Item) error {
	m, err := s.machine()
	if err != nil {
		return err
	}
	if d.CandidateID == 0 || it.RecordingID == 0 {
		return nil
	}
	return sweepBlockCandidate(ctx, m.DB(), d.CandidateID, it.RecordingID, it.TrackID, 0, m.Now())
}
