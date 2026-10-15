package musicengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// StagingExpiry is how long a rejected candidate, and the rest of a release whose one track
// was placed, stay in staging before they are removed (R10, KTD9).
const StagingExpiry = 7 * 24 * time.Hour

// Staging is the directory tree downloads land in: <root>/<item id>/<candidate id>/. It lives
// on the downloads volume, never inside the library (R10, KTD9). Every removal goes through
// removeDir, which refuses anything outside the root and anything under a library root.
type Staging struct {
	root      string
	libraries []string
}

// NewStaging validates the layout. The staging root must be absolute and must neither sit
// inside, equal, nor contain a library root: expiry deletes under the root, so a root that
// could reach the library would let it delete library files.
func NewStaging(root string, libraryRoots ...string) (*Staging, error) {
	if strings.TrimSpace(root) == "" || !filepath.IsAbs(root) {
		return nil, fmt.Errorf("musicengine staging: root %q must be an absolute path", root)
	}
	if len(libraryRoots) == 0 {
		return nil, errors.New("musicengine staging: at least one library root is needed so removals can be fenced off from it")
	}
	s := &Staging{root: filepath.Clean(root)}
	for _, l := range libraryRoots {
		if strings.TrimSpace(l) == "" || !filepath.IsAbs(l) {
			return nil, fmt.Errorf("musicengine staging: library root %q must be an absolute path", l)
		}
		l = filepath.Clean(l)
		if stagingInside(s.root, l) || stagingInside(l, s.root) {
			return nil, fmt.Errorf("musicengine staging: root %s overlaps library root %s", s.root, l)
		}
		s.libraries = append(s.libraries, l)
	}
	return s, nil
}

// Root is the staging root.
func (s *Staging) Root() string { return s.root }

// CandidateDir is where one candidate's download lives.
func (s *Staging) CandidateDir(itemID, candID int64) string {
	return filepath.Join(s.root, strconv.FormatInt(itemID, 10), strconv.FormatInt(candID, 10))
}

func (s *Staging) ensureCandidateDir(itemID, candID int64) (string, error) {
	dir := s.CandidateDir(itemID, candID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating staging dir %s: %w", dir, err)
	}
	return dir, nil
}

// stagingInside reports whether child is parent or below it (lexically).
func stagingInside(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// stagingReal resolves symlinks for the part of path that exists, so a link inside staging
// that points at the library cannot be used to delete through it.
func stagingReal(path string) string {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	return filepath.Clean(path)
}

// removeDir deletes path recursively when, and only when, it is strictly below the staging
// root and not under any library root (by name or after resolving links). A refusal is an
// error, never a silent skip.
func (s *Staging) removeDir(path string) error {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return fmt.Errorf("staging: refusing to remove relative path %q", path)
	}
	if path == s.root || !stagingInside(s.root, path) {
		return fmt.Errorf("staging: refusing to remove %s: not strictly inside %s", path, s.root)
	}
	realRoot, real := stagingReal(s.root), stagingReal(path)
	if real == realRoot || !stagingInside(realRoot, real) {
		return fmt.Errorf("staging: refusing to remove %s: resolves to %s, outside %s", path, real, realRoot)
	}
	for _, lib := range s.libraries {
		if stagingInside(lib, path) || stagingInside(stagingReal(lib), real) {
			return fmt.Errorf("staging: refusing to remove %s: under library root %s", path, lib)
		}
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("removing staging path %s: %w", path, err)
	}
	return nil
}

// StagingReport is what one expiry run did.
type StagingReport struct {
	RemovedCandidates int      // candidate dirs removed because their row expired
	RemovedOrphans    int      // dirs with no candidate row, older than the expiry
	Errors            []string // anything that could not be removed, with why
}

// ExpireStaging removes staging remainders (R10, KTD9): the directory of every candidate
// whose expiry has passed and that is either rejected or belongs to a finished item, and any
// directory that no candidate row owns once it is older than StagingExpiry. A chosen
// candidate of an item that is still active (for example waiting for Blair, KTD12) stays.
// Nothing under a library root is ever touched. The scheduler calls it daily.
func (a *Acquire) ExpireStaging(ctx context.Context) (StagingReport, error) {
	return a.staging.expire(ctx, a.db, a.now(), a.forgetClientJob)
}

func (s *Staging) expire(ctx context.Context, db *sql.DB, now time.Time, forget func(ctx context.Context, lane, key string)) (StagingReport, error) {
	var rep StagingReport
	rows, err := db.QueryContext(ctx,
		`SELECT c.id, c.item_id, c.lane, c.state, c.client_job_id, i.state
		 FROM mus_candidates c JOIN mus_items i ON i.id = c.item_id
		 WHERE c.expires_at > 0 AND c.expires_at <= ?`, now.Unix())
	if err != nil {
		return rep, fmt.Errorf("listing expired staging candidates: %w", err)
	}
	type expired struct {
		id, itemID                  int64
		lane, state, jobID, itemSta string
	}
	var due []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.id, &e.itemID, &e.lane, &e.state, &e.jobID, &e.itemSta); err != nil {
			_ = rows.Close()
			return rep, fmt.Errorf("scanning expired candidate: %w", err)
		}
		due = append(due, e)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return rep, err
	}
	_ = rows.Close()

	for _, e := range due {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		if e.state != "rejected" && !State(e.itemSta).Terminal() {
			continue // a staged candidate of a live item keeps its files (KTD12)
		}
		if e.jobID != "" && forget != nil {
			forget(ctx, e.lane, e.jobID)
		}
		if err := s.removeDir(s.CandidateDir(e.itemID, e.id)); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("candidate %d: %v", e.id, err))
			continue
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE mus_candidates SET staging_path = '', expires_at = 0 WHERE id = ?`, e.id); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("candidate %d: clearing row: %v", e.id, err))
			continue
		}
		rep.RemovedCandidates++
		_ = os.Remove(filepath.Join(s.root, strconv.FormatInt(e.itemID, 10))) // only succeeds when empty
	}
	if err := s.sweepOrphans(ctx, db, now, &rep); err != nil {
		return rep, err
	}
	if len(rep.Errors) > 0 {
		return rep, fmt.Errorf("staging expiry left %d paths: %s", len(rep.Errors), strings.Join(rep.Errors, "; "))
	}
	return rep, nil
}

// sweepOrphans removes <root>/<item>/<cand> directories that no candidate row owns and that
// have not been written to for StagingExpiry (a crash between creating the dir and writing
// the row, or a row deleted with its item).
func (s *Staging) sweepOrphans(ctx context.Context, db *sql.DB, now time.Time, rep *StagingReport) error {
	items, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading staging root %s: %w", s.root, err)
	}
	cutoff := now.Add(-StagingExpiry)
	for _, it := range items {
		itemID, perr := strconv.ParseInt(it.Name(), 10, 64)
		if perr != nil || !it.IsDir() {
			continue // not one of ours
		}
		itemDir := filepath.Join(s.root, it.Name())
		cands, err := os.ReadDir(itemDir)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("reading %s: %v", itemDir, err))
			continue
		}
		for _, c := range cands {
			candID, perr := strconv.ParseInt(c.Name(), 10, 64)
			if perr != nil || !c.IsDir() {
				continue
			}
			var n int
			if err := db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM mus_candidates WHERE id = ? AND item_id = ?`, candID, itemID).Scan(&n); err != nil {
				return fmt.Errorf("checking owner of staging dir %s: %w", c.Name(), err)
			}
			if n > 0 {
				continue
			}
			info, err := c.Info()
			if err != nil || info.ModTime().After(cutoff) {
				continue
			}
			if err := s.removeDir(filepath.Join(itemDir, c.Name())); err != nil {
				rep.Errors = append(rep.Errors, err.Error())
				continue
			}
			rep.RemovedOrphans++
		}
		_ = os.Remove(itemDir)
	}
	return nil
}

// stagingAudioExts are the file types a release's track can be in. Lossless ones plus the
// m4a container (ALAC); the checker decides whether the audio inside is what it claims.
var stagingAudioExts = map[string]bool{".flac": true, ".wav": true, ".ape": true, ".wv": true, ".m4a": true}

// stagingVersionWords are file-name words that mark a different recording of a song. A file
// carrying one is not the target unless the request itself is that version (R6).
var stagingVersionWords = []string{"live", "acoustic", "remix", "demo", "instrumental", "karaoke", "unplugged", "reprise", "cover"}

// stagingFindTrack picks the file in a release directory that is the requested recording.
// A file is a match when its name holds the title as whole words (a leading track number
// does not matter, and a title that is itself a number still matches); of several matches the one with the fewest extra words wins. A file naming another
// version (live, remix, ...) is skipped unless the request is that version. When nothing
// matches it says so; it never falls back to "the only file".
func stagingFindTrack(dir, title, versionHint string) (string, error) {
	want := acqNorm(title)
	if want == "" {
		return "", errors.New("recording has no title to look for")
	}
	hint := " " + acqNorm(versionHint) + " " + acqNorm(title) + " "
	type match struct {
		path  string
		extra int
	}
	var best *match
	var files int
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !stagingAudioExts[strings.ToLower(filepath.Ext(p))] {
			return nil
		}
		files++
		name := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
		words := strings.Fields(acqNorm(name))
		joined := " " + strings.Join(words, " ") + " "
		if !strings.Contains(joined, " "+want+" ") {
			return nil
		}
		for _, v := range stagingVersionWords {
			if strings.Contains(joined, " "+v+" ") && !strings.Contains(hint, " "+v+" ") {
				return nil
			}
		}
		extra := len(words) - len(strings.Fields(want))
		if best == nil || extra < best.extra {
			best = &match{p, extra}
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("reading release in %s: %w", dir, err)
	}
	if best == nil {
		return "", fmt.Errorf("track %q not found among %d audio files in the release", title, files)
	}
	return best.path, nil
}

// acqNorm lowercases and reduces every run of non-alphanumerics to one space.
func acqNorm(s string) string {
	var b strings.Builder
	space := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r > 127:
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		default:
			space = true
		}
	}
	return b.String()
}
