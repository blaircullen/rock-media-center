package musicengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/tristenlammi/arrmada/internal/audiocheck"
	"github.com/tristenlammi/arrmada/internal/store"
)

// Undo (R25, AE10). Undoing swap N puts back the version that swap replaced. It runs the same
// journal as a swap, kind `undo`: the quarantined version is the incoming file and the file
// now in the library is the incumbent, which is quarantined in turn, so an undo loses nothing
// either. Afterwards the candidate the swap had put in is blocked for the recording, so it
// cannot come back.
//
// It refuses, with a reason a person can read, when:
//   - a later swap has changed the track since (Blair undoes that one first), with the later
//     swap's id in UndoRefusal.LaterSwapID;
//   - the old version was cleared from quarantine (R34);
//   - the library file is no longer the version the swap put there (something else changed it);
//   - the track is playing right now.

// ErrUndoRefused is matched by errors.Is for every *UndoRefusal.
var ErrUndoRefused = errors.New("musicengine: undo refused")

// UndoRefusal is why an undo did not run. Nothing was changed.
type UndoRefusal struct {
	Reason string
	// LaterSwapID is the newer swap that blocks the undo, 0 if that is not the reason.
	LaterSwapID int64
	// Cleared is true when the old version was cleared from quarantine.
	Cleared bool
}

func (e *UndoRefusal) Error() string { return "musicengine: undo refused: " + e.Reason }

// Is makes errors.Is(err, ErrUndoRefused) true.
func (e *UndoRefusal) Is(target error) bool { return target == ErrUndoRefused }

func undoRefuse(format string, a ...any) error {
	return &UndoRefusal{Reason: fmt.Sprintf(format, a...)}
}

// UndoResult says what an undo did.
type UndoResult struct {
	// UndoID is the journal row of the undo.
	UndoID int64
	// Confirmed is true once Plex lists the restored file and the rating and playlists were
	// read back. False means the file is back in place and Plex has not caught up yet; Resume
	// (run by the sweep) finishes it.
	Confirmed bool
}

// Undo reverses one completed swap.
func (s *Swaps) Undo(ctx context.Context, swapID int64) (UndoResult, error) {
	m, err := s.machine()
	if err != nil {
		return UndoResult{}, err
	}
	sw, err := s.p.jr.Get(ctx, swapID)
	if err != nil {
		return UndoResult{}, undoRefuse("swap %d does not exist", swapID)
	}
	// One undo of a track at a time (a double click, two tabs). The lock name differs from the
	// destination lock Stage, Advance and confirm take inside, so it nests without deadlock.
	unlockUndo, err := s.p.lock(ctx, "undo:"+sw.NewPath)
	if err != nil {
		return UndoResult{}, err
	}
	defer unlockUndo()
	if sw, err = s.p.jr.Get(ctx, swapID); err != nil {
		return UndoResult{}, err
	}
	switch {
	case sw.Kind != JournalSwap || sw.Phase < PhasePlexConfirmed:
		return UndoResult{}, undoRefuse("journal row %d is not a completed swap", swapID)
	case sw.UndoneAt != 0:
		return UndoResult{}, undoRefuse("swap %d was already undone", swapID)
	}

	// A later swap on the same track blocks the undo (AE10). A swap that is still running
	// counts; one that was undone, rolled back or stopped does not.
	var later int64
	err = m.DB().QueryRowContext(ctx,
		`SELECT id FROM mus_swap_journal
		 WHERE kind = ? AND id > ? AND (track_id = ? OR new_path = ?) AND undone_at = 0 AND (phase >= ? OR closed_at = 0)
		 ORDER BY id LIMIT 1`, JournalSwap, swapID, nullID(sw.TrackID), sw.NewPath, PhasePlexConfirmed).Scan(&later)
	switch {
	case err == nil:
		return UndoResult{}, &UndoRefusal{
			Reason:      fmt.Sprintf("swap %d changed this track after swap %d; undo swap %d first", later, swapID, later),
			LaterSwapID: later,
		}
	case !errors.Is(err, sql.ErrNoRows):
		return UndoResult{}, fmt.Errorf("looking for later swaps of %s: %w", sw.NewPath, err)
	}

	if sw.QuarantineID == 0 {
		return UndoResult{}, undoRefuse("swap %d has no quarantined version recorded", swapID)
	}
	qe, err := s.cfg.Quarantine.Entry(ctx, sw.QuarantineID)
	if err != nil {
		return UndoResult{}, err
	}
	if qe.ClearedAt != 0 {
		return UndoResult{}, &UndoRefusal{
			Reason:  fmt.Sprintf("the old version was cleared from quarantine on %s", time.Unix(qe.ClearedAt, 0).UTC().Format("2006-01-02")),
			Cleared: true,
		}
	}
	if err := s.cfg.Quarantine.Inside(qe.Path); err != nil {
		return UndoResult{}, err
	}
	qsha, err := placeSHA256(qe.Path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return UndoResult{}, undoRefuse("the quarantined file %s is missing", qe.Path)
	case err != nil:
		return UndoResult{}, fmt.Errorf("checksumming the quarantined file %s: %w", qe.Path, err)
	case qsha != sw.QuarantineSHA:
		return UndoResult{}, undoRefuse("the quarantined file %s no longer has the checksum recorded for it", qe.Path)
	}

	if err := s.p.layout.Check(); err != nil {
		return UndoResult{}, err
	}
	if err := s.p.layout.Inside(sw.NewPath); err != nil {
		return UndoResult{}, err
	}
	cur, err := placeSHA256(sw.NewPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return UndoResult{}, undoRefuse("the library file %s is missing", sw.NewPath)
	case err != nil:
		return UndoResult{}, fmt.Errorf("checksumming the library file %s: %w", sw.NewPath, err)
	case cur != sw.PublishedSHA:
		return UndoResult{}, undoRefuse("the library file is no longer the version swap %d put there, so it is left alone", swapID)
	}

	track, err := swapLoadTrack(ctx, m.DB(), sw.TrackID)
	if err != nil {
		return UndoResult{}, undoRefuse("the library track of swap %d is gone (%v)", swapID, err)
	}
	playing, err := s.isPlaying(ctx, track.RatingKey)
	if err != nil {
		return UndoResult{}, err
	}
	if playing {
		return UndoResult{}, undoRefuse("the track is playing right now; try again when it stops")
	}
	snap, err := s.snapshot(ctx, track)
	if err != nil {
		return UndoResult{}, err
	}
	if err := s.record(ctx, m, sw.RecordingID, track, cur, snap); err != nil {
		return UndoResult{}, err
	}
	members, err := swapEncodeMembers(snap.Members)
	if err != nil {
		return UndoResult{}, err
	}

	row, err := s.p.Stage(ctx, StageSpec{
		Kind: JournalUndo, TrackID: sw.TrackID, RecordingID: sw.RecordingID,
		Source: qe.Path, SourceSHA: sw.QuarantineSHA, Dest: sw.NewPath, IncumbentSHA: cur,
		QuarantinePath:    s.cfg.Quarantine.PathFor(sw.TrackID, "undo-"+strconv.FormatInt(swapID, 10), cur, sw.NewPath),
		ExpectFingerprint: sw.OldFingerprint, OldFingerprint: sw.NewFingerprint,
		RatingBefore: snap.Rating, MembershipsBefore: members, PreviousSwapID: swapID,
	})
	if err != nil {
		var ve *VerifyError
		if errors.As(err, &ve) {
			return UndoResult{}, undoRefuse("the quarantined version failed its checks (%s); the library file was not touched", ve.Reason)
		}
		return UndoResult{UndoID: row.ID}, err
	}
	res := UndoResult{UndoID: row.ID}
	out, err := s.p.Advance(ctx, row)
	if err != nil {
		return res, fmt.Errorf("undo %d is open and will be retried: %w", row.ID, err)
	}
	if out == OutcomePending {
		return res, fmt.Errorf("undo %d is open and will finish when the track stops playing: %w", row.ID, ErrPlaybackPending)
	}
	if out != OutcomeAdvanced {
		last, gerr := s.p.jr.Get(ctx, row.ID)
		if gerr != nil {
			return res, gerr
		}
		return res, fmt.Errorf("undo %d %s: %s", row.ID, out, last.LastError)
	}
	confirmed, err := s.confirmItemless(ctx, row.ID, true)
	res.Confirmed = confirmed
	return res, err
}

// confirmItemless takes a published, item-less swap or undo row to plex_confirmed: ask Plex,
// read the file's facts, finalize (rating and memberships) and close the row. poll waits a
// few seconds for Plex to index. It reports whether the row is now confirmed.
func (s *Swaps) confirmItemless(ctx context.Context, rowID int64, poll bool) (bool, error) {
	row, err := s.p.jr.Get(ctx, rowID)
	if err != nil {
		return false, err
	}
	if row.ClosedAt != 0 && row.Phase >= PhasePlexConfirmed {
		return true, s.undoSettle(ctx, row)
	}
	if row.Phase != PhasePublished {
		return false, nil
	}
	tags, err := s.p.readTags(ctx, row.NewPath)
	if err != nil {
		return false, fmt.Errorf("reading tags of %s: %w", row.NewPath, err)
	}
	key, indexed, err := s.p.plexIndexed(ctx, row, tags, poll)
	if err != nil || !indexed {
		return false, err // not indexed yet: the row stays at published for the next run
	}
	res := s.cfg.Checker.Check(ctx, row.NewPath)
	var facts *placeFacts
	if res.Verdict != audiocheck.Blind {
		facts = undoFacts(res, row.PublishedSHA)
	} // a blind read leaves the facts for the next library check; the file itself was verified in place
	if err := s.p.confirm(ctx, row, tags, key, facts, false); err != nil {
		return false, err
	}
	done, err := s.p.jr.Get(ctx, rowID)
	if err != nil {
		return false, err
	}
	return true, s.undoSettle(ctx, done)
}

func undoFacts(r audiocheck.Result, sha string) *placeFacts {
	f := &placeFacts{
		Codec: r.Facts.Codec, Container: r.Facts.Container, BitDepth: r.Facts.BitDepth, SampleRate: r.Facts.SampleRate,
		Channels: r.Facts.Channels, Bitrate: int(r.Facts.BitrateKbps + 0.5), DurationMs: int64(r.Facts.DurationSec * 1000),
		Verdict: string(r.Verdict), Fingerprint: r.Fingerprint, SHA256: sha,
	}
	if r.Signature != nil {
		f.TopHz = int(r.Signature.TopHz)
		f.FloorDB = sql.NullFloat64{Float64: r.Signature.FloorDB, Valid: true}
	}
	if r.LoudnessLUFS != nil {
		f.LoudnessLUFS = sql.NullFloat64{Float64: *r.LoudnessLUFS, Valid: true}
	}
	return f
}

// undoSettle marks the swap an undo reversed as undone and blocks the candidate it had put
// in (recording plus decoded-audio fingerprint, so the block survives a Plex re-import). It
// is a no-op for any row that is not a completed undo, and safe to repeat.
func (s *Swaps) undoSettle(ctx context.Context, undo JournalRow) error {
	if undo.Kind != JournalUndo || undo.Phase < PhasePlexConfirmed || undo.PreviousSwapID == 0 {
		return nil
	}
	m, err := s.machine()
	if err != nil {
		return err
	}
	sw, err := s.p.jr.Get(ctx, undo.PreviousSwapID)
	if err != nil {
		return err
	}
	return store.WithTx(ctx, m.DB(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE mus_swap_journal SET undone_at = ?, undone_by_id = ? WHERE id = ? AND undone_at = 0`,
			m.Now().Unix(), undo.ID, sw.ID); err != nil {
			return fmt.Errorf("marking swap %d undone: %w", sw.ID, err)
		}
		if sw.RecordingID == 0 || sw.NewFingerprint == "" {
			s.log.Warn("musicengine undo: the undone candidate could not be blocked (no recording or fingerprint recorded)", "swap", sw.ID)
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO mus_blocked_candidates (recording_id, track_id, pcm_fingerprint, swap_id, created_at)
			 VALUES (?, ?, ?, ?, ?)`, sw.RecordingID, nullID(sw.TrackID), sw.NewFingerprint, sw.ID, m.Now().Unix()); err != nil {
			return fmt.Errorf("blocking the candidate of swap %d: %w", sw.ID, err)
		}
		return nil
	})
}

// Resume finishes undo work a call left behind: an open undo row (a Plex hiccup, an error
// after the file was in place) is taken forward, and a completed undo whose swap was never
// marked undone is settled. The boot reconciler covers a crash; this covers a running process.
func (s *Swaps) Resume(ctx context.Context) error {
	m, err := s.machine()
	if err != nil {
		return err
	}
	rows, err := s.p.jr.Unsettled(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range rows {
		// Item-less rows have no item step to finish them: an undo, and an album track
		// placed without an item that Plex had not indexed yet (finding 15). A row with an
		// item is finished by that item's own step.
		if r.ItemID != 0 || (r.Kind != JournalUndo && r.Kind != JournalPlace) {
			continue
		}
		if r.Phase < PhasePublished {
			out, err := s.p.Advance(ctx, r)
			if errors.Is(err, ErrSpaceWait) {
				s.log.Info("musicengine resume: row waits for disk space", "row", r.ID, "err", err)
				continue
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("resuming %s row %d: %w", r.Kind, r.ID, err))
				continue
			}
			if out != OutcomeAdvanced {
				continue // pending (a playing track), rolled back or stopped: nothing to confirm
			}
		}
		confirmed, err := s.confirmItemless(ctx, r.ID, false)
		if err != nil {
			errs = append(errs, fmt.Errorf("confirming %s row %d: %w", r.Kind, r.ID, err))
		} else if !confirmed {
			s.log.Debug("musicengine resume: row is pending, Plex has not indexed it yet", "row", r.ID, "path", r.NewPath)
		}
	}
	// Completed undos whose swaps are not marked.
	done, err := journalList(ctx, m.DB(),
		`WHERE kind = ? AND phase >= ? AND closed_at > 0 AND previous_swap_id IN (SELECT id FROM mus_swap_journal WHERE undone_at = 0)`,
		JournalUndo, PhasePlexConfirmed)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, r := range done {
		if err := s.undoSettle(ctx, r); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
