package musicengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/tristenlammi/arrmada/internal/store"
)

// The swap journal (KTD7, KTD18). One row per placement, swap or undo. phase is the last
// phase recorded as VERIFIED, never intent: a phase that was started but not recorded is
// redone or undone from what is on disk. U11 uses this same API for swaps and undo.

// Journal kinds.
const (
	JournalPlace = "place"
	JournalSwap  = "swap"
	JournalUndo  = "undo"
)

// Journal phases.
const (
	PhaseNone               = 0
	PhaseStaged             = 1 // candidate passed R12 in staging
	PhaseTempVerified       = 2 // temp file in the target folder is tagged, checksummed and fully decoded
	PhaseQuarantineVerified = 3 // quarantine copy of the incumbent matches the incumbent's checksum
	PhasePublished          = 4 // destination holds the new file's checksum
	PhasePlexConfirmed      = 5 // Plex lists the track (and U11: rating and memberships read back)
)

// ErrJournalBusy means another open row already owns the destination path (R23). It is a
// retryable condition, not a crash.
var ErrJournalBusy = errors.New("musicengine: another journal row is open for that path")

// JournalRow is one row of mus_swap_journal. Ids are 0 when unset.
type JournalRow struct {
	ID          int64
	Kind        string
	ItemID      int64
	TrackID     int64
	CandidateID int64
	RecordingID int64

	OldPath, NewPath, TempPath     string
	QuarantineID                   int64
	QuarantinePath                 string
	Phase                          int
	StagedSHA, TempSHA             string
	IncumbentSHA, QuarantineSHA    string
	PublishedSHA                   string
	NewFingerprint, OldFingerprint string
	RatingBefore                   *float64
	MembershipsBefore              string
	PreviousSwapID                 int64
	UndoneAt, UndoneByID           int64
	LastError                      string
	StartedAt, PhaseAt, ClosedAt   int64
}

// Open reports whether the row is unsettled.
func (r JournalRow) Open() bool { return r.ClosedAt == 0 }

const journalCols = `id, kind, COALESCE(item_id,0), COALESCE(track_id,0), COALESCE(candidate_id,0),
	COALESCE(recording_id,0), old_path, new_path, temp_path, COALESCE(quarantine_id,0), quarantine_path,
	phase, staged_sha256, temp_sha256, incumbent_sha256, quarantine_sha256, published_sha256,
	new_pcm_fingerprint, old_pcm_fingerprint, rating_before, memberships_before,
	COALESCE(previous_swap_id,0), undone_at, COALESCE(undone_by_id,0), last_error,
	started_at, phase_at, closed_at`

func scanJournal(r rowScanner) (JournalRow, error) {
	var j JournalRow
	var rating sql.NullFloat64
	err := r.Scan(&j.ID, &j.Kind, &j.ItemID, &j.TrackID, &j.CandidateID, &j.RecordingID,
		&j.OldPath, &j.NewPath, &j.TempPath, &j.QuarantineID, &j.QuarantinePath,
		&j.Phase, &j.StagedSHA, &j.TempSHA, &j.IncumbentSHA, &j.QuarantineSHA, &j.PublishedSHA,
		&j.NewFingerprint, &j.OldFingerprint, &rating, &j.MembershipsBefore,
		&j.PreviousSwapID, &j.UndoneAt, &j.UndoneByID, &j.LastError,
		&j.StartedAt, &j.PhaseAt, &j.ClosedAt)
	if rating.Valid {
		v := rating.Float64
		j.RatingBefore = &v
	}
	return j, err
}

// JournalBegin is what a new row records at phase 0: intent, not a completed phase.
type JournalBegin struct {
	Kind        string
	ItemID      int64
	TrackID     int64
	CandidateID int64
	RecordingID int64
	// OldPath is the incumbent (empty for a new track); NewPath is the destination in the
	// library. For a swap or undo the two are equal (KTD8).
	OldPath, NewPath string
	// IncumbentSHA is what the destination must hold before the incumbent is replaced.
	IncumbentSHA string
	// QuarantinePath is where the incumbent's copy goes (swap and undo).
	QuarantinePath                 string
	NewFingerprint, OldFingerprint string
	RatingBefore                   *float64
	MembershipsBefore              string
	PreviousSwapID                 int64
}

// Journal reads and writes mus_swap_journal.
type Journal struct {
	db  *sql.DB
	now func() time.Time
}

// NewJournal builds the journal over a database.
func NewJournal(db *sql.DB, now func() time.Time) *Journal {
	if now == nil {
		now = time.Now
	}
	return &Journal{db: db, now: now}
}

// journalTempName is the deterministic temp name for a row: dot-prefixed, so Plex ignores it,
// and carrying the row id, so a leftover is always attributable.
func journalTempName(id int64, dest string) string {
	return filepath.Join(filepath.Dir(dest), fmt.Sprintf(".rmc-tmp-%d-%s", id, filepath.Base(dest)))
}

// journalTempPrefix is the prefix every temp of row id starts with.
func journalTempPrefix(id int64) string { return fmt.Sprintf(".rmc-tmp-%d-", id) }

// Start opens a row at phase 0. new_path and temp_path are written now (intent) so the
// reconciler can find stray files. A destination that already has an open row returns
// ErrJournalBusy.
func (j *Journal) Start(ctx context.Context, b JournalBegin) (JournalRow, error) {
	if b.NewPath == "" {
		return JournalRow{}, errors.New("journal start: no destination path")
	}
	switch b.Kind {
	case JournalPlace, JournalSwap, JournalUndo:
	default:
		return JournalRow{}, fmt.Errorf("journal start: unknown kind %q", b.Kind)
	}
	now := j.now().Unix()
	var row JournalRow
	err := store.WithTx(ctx, j.db, func(tx *sql.Tx) error {
		var open int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mus_swap_journal WHERE new_path = ? AND closed_at = 0`, b.NewPath).Scan(&open); err != nil {
			return fmt.Errorf("checking open journal rows for %s: %w", b.NewPath, err)
		}
		if open > 0 {
			return fmt.Errorf("%w: %s", ErrJournalBusy, b.NewPath)
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO mus_swap_journal (kind, item_id, track_id, candidate_id, recording_id, old_path, new_path,
			        quarantine_path, incumbent_sha256, new_pcm_fingerprint, old_pcm_fingerprint, rating_before,
			        memberships_before, previous_swap_id, started_at, phase_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			b.Kind, nullID(b.ItemID), nullID(b.TrackID), nullID(b.CandidateID), nullID(b.RecordingID),
			b.OldPath, b.NewPath, b.QuarantinePath, b.IncumbentSHA, b.NewFingerprint, b.OldFingerprint,
			b.RatingBefore, b.MembershipsBefore, nullID(b.PreviousSwapID), now, now)
		if err != nil {
			return fmt.Errorf("opening journal row for %s: %w", b.NewPath, err)
		}
		id, _ := res.LastInsertId()
		if _, err := tx.ExecContext(ctx, `UPDATE mus_swap_journal SET temp_path = ? WHERE id = ?`, journalTempName(id, b.NewPath), id); err != nil {
			return fmt.Errorf("naming temp file of journal row %d: %w", id, err)
		}
		row, err = journalGet(ctx, tx, id)
		return err
	})
	return row, err
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// PhaseData is what a phase records once verified.
type PhaseData struct {
	StagedSHA, TempSHA, QuarantineSHA, PublishedSHA string
	QuarantineID                                    int64
}

// Record marks a phase complete after its result was verified. It only moves forward
// (phase < new) and only on an open row; recording a phase the row already passed is a
// no-op so a replayed step is harmless. advanced says whether the row moved. q lets the
// caller record in the same transaction as the item's state change.
func (j *Journal) Record(ctx context.Context, q store.Execer, id int64, phase int, d PhaseData) (advanced bool, err error) {
	var col string
	switch phase {
	case PhaseStaged:
		col = "staged_sha256"
	case PhaseTempVerified:
		col = "temp_sha256"
	case PhaseQuarantineVerified:
		col = "quarantine_sha256"
	case PhasePublished:
		col = "published_sha256"
	case PhasePlexConfirmed:
		col = ""
	default:
		return false, fmt.Errorf("journal record: phase %d cannot be recorded", phase)
	}
	val := map[int]string{PhaseStaged: d.StagedSHA, PhaseTempVerified: d.TempSHA, PhaseQuarantineVerified: d.QuarantineSHA, PhasePublished: d.PublishedSHA}[phase]
	if col != "" && val == "" {
		return false, fmt.Errorf("journal record: phase %d needs a checksum", phase)
	}
	set := "phase = ?, phase_at = ?"
	args := []any{phase, j.now().Unix()}
	if col != "" {
		set += ", " + col + " = ?"
		args = append(args, val)
	}
	if phase == PhaseQuarantineVerified && d.QuarantineID != 0 {
		set += ", quarantine_id = ?"
		args = append(args, d.QuarantineID)
	}
	args = append(args, id, phase)
	res, err := q.ExecContext(ctx, `UPDATE mus_swap_journal SET `+set+` WHERE id = ? AND closed_at = 0 AND phase < ?`, args...)
	if err != nil {
		return false, fmt.Errorf("recording phase %d on journal row %d: %w", phase, id, err)
	}
	n, _ := res.RowsAffected()
	if n == 1 {
		return true, nil
	}
	cur, gerr := journalGet(ctx, q, id)
	if gerr != nil {
		return false, gerr
	}
	if cur.ClosedAt != 0 || cur.Phase >= phase {
		return false, nil
	}
	return false, fmt.Errorf("journal row %d did not take phase %d", id, phase)
}

// Close settles a row: completed (phase 5, empty reason), rolled back or refused (the
// reason is kept in last_error). Closing twice changes nothing.
func (j *Journal) Close(ctx context.Context, q store.Execer, id int64, reason string) error {
	_, err := q.ExecContext(ctx,
		`UPDATE mus_swap_journal SET closed_at = ?, last_error = CASE WHEN ? <> '' THEN ? ELSE last_error END
		 WHERE id = ? AND closed_at = 0`, j.now().Unix(), reason, clip(reason, 500), id)
	if err != nil {
		return fmt.Errorf("closing journal row %d: %w", id, err)
	}
	return nil
}

// Note keeps a remark on an open row (for example why chown was skipped) without closing it.
func (j *Journal) Note(ctx context.Context, id int64, note string) error {
	_, err := j.db.ExecContext(ctx,
		`UPDATE mus_swap_journal SET last_error = CASE WHEN last_error = '' THEN ? ELSE last_error || '; ' || ? END WHERE id = ?`,
		clip(note, 300), clip(note, 300), id)
	if err != nil {
		return fmt.Errorf("noting journal row %d: %w", id, err)
	}
	return nil
}

// Get reads one row.
func (j *Journal) Get(ctx context.Context, id int64) (JournalRow, error) {
	return journalGet(ctx, j.db, id)
}

func journalGet(ctx context.Context, q store.Execer, id int64) (JournalRow, error) {
	r, err := scanJournal(q.QueryRowContext(ctx, `SELECT `+journalCols+` FROM mus_swap_journal WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return JournalRow{}, fmt.Errorf("journal row %d not found", id)
	}
	if err != nil {
		return JournalRow{}, fmt.Errorf("reading journal row %d: %w", id, err)
	}
	return r, nil
}

// Unsettled lists every open row, oldest first.
func (j *Journal) Unsettled(ctx context.Context) ([]JournalRow, error) {
	return journalList(ctx, j.db, `WHERE closed_at = 0 ORDER BY id`)
}

// OpenForItem returns the item's open row, if any (the newest).
func (j *Journal) OpenForItem(ctx context.Context, itemID int64) (JournalRow, bool, error) {
	rows, err := journalList(ctx, j.db, `WHERE closed_at = 0 AND item_id = ? ORDER BY id DESC LIMIT 1`, itemID)
	if err != nil || len(rows) == 0 {
		return JournalRow{}, false, err
	}
	return rows[0], true, nil
}

// OpenForPath returns the open row owning a destination path, if any.
func (j *Journal) OpenForPath(ctx context.Context, path string) (JournalRow, bool, error) {
	rows, err := journalList(ctx, j.db, `WHERE closed_at = 0 AND new_path = ? LIMIT 1`, path)
	if err != nil || len(rows) == 0 {
		return JournalRow{}, false, err
	}
	return rows[0], true, nil
}

func journalList(ctx context.Context, q store.Execer, tail string, args ...any) ([]JournalRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+journalCols+` FROM mus_swap_journal `+tail, args...)
	if err != nil {
		return nil, fmt.Errorf("listing journal rows: %w", err)
	}
	defer rows.Close()
	var out []JournalRow
	for rows.Next() {
		r, err := scanJournal(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning journal row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// journalQuarantineRow stores (or finds) the mus_quarantine row for a quarantined version
// and returns its id. The path is unique, so a redo finds the row it made before.
func journalQuarantineRow(ctx context.Context, q store.Execer, trackID int64, path, origin, sha string, size int64, now time.Time) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx,
		`INSERT INTO mus_quarantine (track_id, path, origin_path, sha256, size_bytes, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET sha256 = excluded.sha256, size_bytes = excluded.size_bytes
		 RETURNING id`, nullID(trackID), path, origin, sha, size, now.Unix()).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("storing quarantine row for %s: %w", path, err)
	}
	return id, nil
}

// journalIsTemp reports whether a file name is one of this engine's temp names.
func journalIsTemp(name string) bool { return strings.HasPrefix(name, ".rmc-tmp-") }
