package musicengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Quarantine (KTD9, R24, R34). Every file a swap or undo replaces is kept, one copy per
// replaced version, below the quarantine root:
//
//	<root>/<library track id>/<tag>-<sha12>-<file name>
//
// The tag names the swap (swap-<item id>, undo-<swap id>) so the same bytes quarantined twice
// for two different swaps are two entries. Placement copies the incumbent in and records the
// mus_quarantine row (journalQuarantineRow); this file owns the layout, the share accounting
// U12 reads, and the one way anything leaves: Clear.
//
// Nothing here ever deletes outside the quarantine root, and a row whose path resolves
// outside it is reported and left alone.

// DefaultQuarantineClearAge is the age Blair's clear action defaults to (R34).
const DefaultQuarantineClearAge = 90 * 24 * time.Hour

// QuarantineConfig wires a Quarantine.
type QuarantineConfig struct {
	DB *sql.DB
	// Root is the quarantine root (KTD9); it must already exist.
	Root string
	// Share is the byte budget (R34); 0 means DefaultQuarantineShare. Read fresh on each use.
	Share func() int64
	Now   func() time.Time
	Log   *slog.Logger
}

// Quarantine owns the quarantine root.
type Quarantine struct {
	db    *sql.DB
	root  string
	share func() int64
	now   func() time.Time
	log   *slog.Logger
}

// NewQuarantine builds a Quarantine over an absolute root.
func NewQuarantine(cfg QuarantineConfig) (*Quarantine, error) {
	if cfg.DB == nil {
		return nil, errors.New("musicengine quarantine: no database")
	}
	if cfg.Root == "" || !filepath.IsAbs(cfg.Root) {
		return nil, fmt.Errorf("musicengine quarantine: root %q must be an absolute path", cfg.Root)
	}
	q := &Quarantine{db: cfg.DB, root: filepath.Clean(cfg.Root), share: cfg.Share, now: cfg.Now, log: cfg.Log}
	if q.now == nil {
		q.now = time.Now
	}
	if q.log == nil {
		q.log = slog.New(slog.DiscardHandler)
	}
	return q, nil
}

// Root is the quarantine root.
func (q *Quarantine) Root() string { return q.root }

var quarNameRe = regexp.MustCompile(`[^A-Za-z0-9._ ()'&,+-]+`)

// quarSafeName keeps a file name readable and free of path separators.
func quarSafeName(name string) string {
	name = quarNameRe.ReplaceAllString(filepath.Base(name), "_")
	name = strings.Trim(name, " .")
	if name == "" {
		return "track"
	}
	return name
}

// PathFor is where the version with the given checksum goes. tag names the swap that
// replaces it; it is part of the file name, so two swaps never share an entry.
func (q *Quarantine) PathFor(trackID int64, tag, sha, origin string) string {
	sha12 := sha
	if len(sha12) > 12 {
		sha12 = sha12[:12]
	}
	return filepath.Join(q.root, strconv.FormatInt(trackID, 10), quarSafeName(tag+"-"+sha12+"-"+filepath.Base(origin)))
}

// Inside refuses a path that is not below the quarantine root (symlinks included).
func (q *Quarantine) Inside(path string) error {
	return layoutInside(q.root, path)
}

// QuarantineUsage is the share accounting U12 and the status page read (R34).
type QuarantineUsage struct {
	UsedBytes  int64 // versions not yet cleared
	Versions   int
	ShareBytes int64
	Full       bool // used has reached the share: the sweep pauses
	OldestAt   int64
}

// Usage reports the share.
func (q *Quarantine) Usage(ctx context.Context) (QuarantineUsage, error) {
	var u QuarantineUsage
	err := q.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(size_bytes), 0), COUNT(*), COALESCE(MIN(created_at), 0) FROM mus_quarantine WHERE cleared_at = 0`).
		Scan(&u.UsedBytes, &u.Versions, &u.OldestAt)
	if err != nil {
		return u, fmt.Errorf("reading quarantine usage: %w", err)
	}
	u.ShareBytes = DefaultQuarantineShare
	if q.share != nil {
		if s := q.share(); s > 0 {
			u.ShareBytes = s
		}
	}
	u.Full = u.UsedBytes >= u.ShareBytes
	return u, nil
}

// QuarantineEntry is one row of mus_quarantine.
type QuarantineEntry struct {
	ID        int64
	TrackID   int64
	Path      string
	Origin    string
	SHA256    string
	SizeBytes int64
	CreatedAt int64
	ClearedAt int64
}

func quarScan(r rowScanner) (QuarantineEntry, error) {
	var e QuarantineEntry
	err := r.Scan(&e.ID, &e.TrackID, &e.Path, &e.Origin, &e.SHA256, &e.SizeBytes, &e.CreatedAt, &e.ClearedAt)
	return e, err
}

const quarCols = `id, COALESCE(track_id, 0), path, origin_path, sha256, size_bytes, created_at, cleared_at`

// Entry reads one quarantine row.
func (q *Quarantine) Entry(ctx context.Context, id int64) (QuarantineEntry, error) {
	e, err := quarScan(q.db.QueryRowContext(ctx, `SELECT `+quarCols+` FROM mus_quarantine WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return QuarantineEntry{}, fmt.Errorf("quarantine entry %d not found", id)
	}
	if err != nil {
		return QuarantineEntry{}, fmt.Errorf("reading quarantine entry %d: %w", id, err)
	}
	return e, nil
}

// ClearReport says what a clear did.
type ClearReport struct {
	Cleared    int
	FreedBytes int64
	// Missing counts rows whose file was already gone; they are marked cleared all the same.
	Missing int
	// Skipped lists rows left alone and why (outside the root, still needed by an open journal row).
	Skipped []string
}

// Clear removes quarantined versions older than olderThan, marks their rows cleared, and
// touches nothing outside the quarantine root (R34). After it, an undo of a swap whose
// version was cleared is refused with that reason. Versions an unsettled journal row still
// needs are kept. An error on one file leaves its row uncleared and the rest go on.
func (q *Quarantine) Clear(ctx context.Context, olderThan time.Duration) (ClearReport, error) {
	var rep ClearReport
	if olderThan < 0 {
		return rep, errors.New("musicengine quarantine: the age to clear must not be negative")
	}
	if fi, err := os.Stat(q.root); err != nil || !fi.IsDir() {
		return rep, fmt.Errorf("musicengine quarantine: root %s is unreachable (%v); nothing was cleared", q.root, err)
	}
	cutoff := q.now().Add(-olderThan).Unix()
	rows, err := q.db.QueryContext(ctx,
		`SELECT `+quarCols+` FROM mus_quarantine
		 WHERE cleared_at = 0 AND created_at <= ?
		   AND path NOT IN (SELECT quarantine_path FROM mus_swap_journal WHERE closed_at = 0 AND quarantine_path <> '')
		 ORDER BY id`, cutoff)
	if err != nil {
		return rep, fmt.Errorf("listing quarantined versions to clear: %w", err)
	}
	var entries []QuarantineEntry
	for rows.Next() {
		e, err := quarScan(rows)
		if err != nil {
			_ = rows.Close()
			return rep, fmt.Errorf("scanning quarantine row: %w", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return rep, err
	}
	_ = rows.Close()

	var errs []error
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := q.Inside(e.Path); err != nil {
			rep.Skipped = append(rep.Skipped, fmt.Sprintf("%s: %v", e.Path, err))
			continue
		}
		fi, serr := os.Lstat(e.Path)
		switch {
		case serr == nil && !fi.Mode().IsRegular():
			rep.Skipped = append(rep.Skipped, e.Path+": not a regular file, left alone")
			continue
		case serr == nil:
			if err := os.Remove(e.Path); err != nil {
				errs = append(errs, fmt.Errorf("removing quarantined %s: %w", e.Path, err))
				continue
			}
		case errors.Is(serr, fs.ErrNotExist):
			rep.Missing++
		default:
			errs = append(errs, fmt.Errorf("looking at quarantined %s: %w", e.Path, serr))
			continue
		}
		if _, err := q.db.ExecContext(ctx, `UPDATE mus_quarantine SET cleared_at = ? WHERE id = ? AND cleared_at = 0`, q.now().Unix(), e.ID); err != nil {
			errs = append(errs, fmt.Errorf("marking quarantine entry %d cleared (the file is already gone): %w", e.ID, err))
			continue
		}
		rep.Cleared++
		rep.FreedBytes += e.SizeBytes
		// Drop the track's folder when it is now empty; a non-empty one refuses, which is right.
		if dir := filepath.Dir(e.Path); dir != q.root && q.Inside(dir) == nil {
			_ = os.Remove(dir)
		}
	}
	return rep, errors.Join(errs...)
}
