package musicengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tristenlammi/arrmada/internal/download"
	"github.com/tristenlammi/arrmada/internal/indexer"
	"github.com/tristenlammi/arrmada/internal/music"
	"github.com/tristenlammi/arrmada/internal/slskd"
	"github.com/tristenlammi/arrmada/internal/store"
	"github.com/tristenlammi/arrmada/internal/ytaudio"
)

// Lane names, as stored in mus_candidates.lane and mus_lane_status.lane.
const (
	LaneUsenet   = "usenet"
	LaneTorrent  = "torrent"
	LaneSoulseek = "soulseek"
	LaneYouTube  = "youtube"
)

// acqCategoryDefault is the download-client category for engine downloads (KTD1). U15 adds
// the constant to internal/download/categories.go; until then this package keeps its own.
const acqCategoryDefault = "rmc-music"

// Newznab categories searched in order (KTD14): lossless audio, then all audio.
var laneUsenetCategories = []int{3040, 3000}

// laneMaxReleaseBytes drops a release larger than this before it is ever scored: a
// discography or box set is not the release that holds one song.
const laneMaxReleaseBytes = 4 << 30

// laneMaxCandidates caps how many candidates one lane search records.
const laneMaxCandidates = 8

// ---- the building blocks, as small interfaces so tests use fakes ----

// LaneIndexer is internal/indexer.Service's slice the usenet and torrent lanes use.
type LaneIndexer interface {
	List(ctx context.Context) ([]indexer.Indexer, error)
	Search(ctx context.Context, q indexer.SearchQuery) (indexer.SearchResult, error)
	Fetch(ctx context.Context, indexerName, downloadURL string) (indexer.FetchResult, error)
}

// LaneDownloader is internal/download.Service's slice: SABnzbd and qBittorrent through its
// client registry. AddDownload is given the "rmc-music" category and a staging SavePath.
type LaneDownloader interface {
	AddDownload(ctx context.Context, req download.AddRequest) (download.Added, error)
	QueueComplete(ctx context.Context) ([]download.Item, bool, error)
	Remove(ctx context.Context, key string, deleteData bool) error
}

// LaneSlskd is internal/slskd.Client's slice.
type LaneSlskd interface {
	Search(ctx context.Context, query string, opt slskd.SearchOptions) ([]slskd.File, error)
	Enqueue(ctx context.Context, username, filename string, size int64) error
	GetDownload(ctx context.Context, username, filename string) (slskd.Transfer, error)
}

// LaneYouTubeClient is internal/ytaudio.Client's slice. It is used only after Blair's "lossy"
// decision (AcceptLossy).
type LaneYouTubeClient interface {
	Search(ctx context.Context, artist, title string) ([]ytaudio.Candidate, error)
	Download(ctx context.Context, url, dir string) (ytaudio.Result, error)
}

// VPNCheck reports whether the torrent and Soulseek clients are still attached to the VPN
// as recorded at install (KTD20: network_mode container:gluetun, kill switch on). U15 wires
// it to the stored U16 evidence. ok=false or an error means the lane is unavailable; a nil
// VPNCheck means nothing was recorded, which is the same.
type VPNCheck func(ctx context.Context) (ok bool, reason string)

var (
	_ LaneIndexer       = (*indexer.Service)(nil)
	_ LaneDownloader    = (*download.Service)(nil)
	_ LaneSlskd         = (*slskd.Client)(nil)
	_ LaneYouTubeClient = (*ytaudio.Client)(nil)
)

// ---- lane model ----

// laneQuery is what a lane searches for.
type laneQuery struct {
	Artist, Title, Album string
	VersionHint          string
	LengthMs             int
}

func (q laneQuery) text() string { return strings.TrimSpace(q.Artist + " " + q.Title) }

// acqSource is where a candidate came from, stored as JSON in mus_candidates.source_url so
// a restart can start the download without searching again.
type acqSource struct {
	Indexer  string `json:"indexer,omitempty"`
	URL      string `json:"url,omitempty"`
	InfoHash string `json:"info_hash,omitempty"`
	User     string `json:"user,omitempty"`
	Filename string `json:"filename,omitempty"`
	Size     int64  `json:"size,omitempty"`
}

func (s acqSource) encode() string {
	b, err := json.Marshal(s)
	if err != nil {
		return "{}" // a struct of strings and an int cannot fail to marshal
	}
	return string(b)
}

func acqDecodeSource(raw string) (acqSource, error) {
	var s acqSource
	if strings.TrimSpace(raw) == "" {
		return s, nil
	}
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return acqSource{}, fmt.Errorf("candidate source %q is not readable: %w", clip(raw, 80), err)
	}
	return s, nil
}

// laneCandidate is one release (or, for Soulseek, one file) a lane found.
type laneCandidate struct {
	ReleaseName string
	Source      acqSource
	SizeBytes   int64
	Score       int
}

// laneResult is one lane's report: searched (with or without candidates) or unavailable with
// the reason. A lane that could not look is never "searched" (R9, AE13).
type laneResult struct {
	Lane       string
	Searched   bool
	Reason     string // set when !Searched
	Candidates []laneCandidate
}

func laneUnavailable(lane, format string, args ...any) laneResult {
	return laneResult{Lane: lane, Reason: fmt.Sprintf(format, args...)}
}

// Lane base scores keep lane order (R8) even for candidates left over from an earlier pass;
// pre-scoring orders candidates only within a lane (it stays under 1000).
const (
	laneBaseUsenet   = 3000
	laneBaseTorrent  = 2000
	laneBaseSoulseek = 1000
)

// laneSearcher is one source. search never returns an error: a failure is an unavailable
// result carrying the reason.
type laneSearcher interface {
	name() string
	search(ctx context.Context, q laneQuery) laneResult
}

// ---- usenet and torrent lanes: both search the aggregated indexer service ----

type laneIndexed struct {
	lane      string
	transport indexer.Transport
	base      int
	idx       LaneIndexer
	vpn       VPNCheck // nil for usenet; required for torrents (R8)
	cats      []int
}

func (l *laneIndexed) name() string { return l.lane }

func (l *laneIndexed) search(ctx context.Context, q laneQuery) laneResult {
	if l.idx == nil {
		return laneUnavailable(l.lane, "%s lane is not configured", l.lane)
	}
	if l.transport == indexer.TransportTorrent {
		if why := laneVPNProblem(ctx, l.vpn); why != "" {
			return laneUnavailable(l.lane, "%s", why)
		}
	}
	all, err := l.idx.List(ctx)
	if err != nil {
		return laneUnavailable(l.lane, "reading indexers: %v", err)
	}
	names := map[string]bool{}
	for _, ix := range all {
		if ix.Enabled && ix.Serves(indexer.MediaMusic) && ix.Transport() == l.transport {
			names[ix.Name] = true
		}
	}
	if len(names) == 0 {
		return laneUnavailable(l.lane, "no enabled %s indexer serves music", l.transport)
	}

	var issues []string
	for _, cat := range l.cats {
		sr, err := l.idx.Search(ctx, indexer.SearchQuery{
			Text: q.text(), Categories: []int{cat}, MediaType: indexer.MediaMusic, Limit: 100,
		})
		if err != nil {
			var all *indexer.AllFailedError
			if errors.As(err, &all) {
				issues = append(issues, laneIssueText(all.Errors, all.Skipped, names)...)
				if len(issues) == 0 {
					issues = append(issues, err.Error())
				}
			} else {
				issues = append(issues, err.Error())
			}
			continue
		}
		issues = append(issues, laneIssueText(sr.Errors, sr.Skipped, names)...)
		cands := l.score(sr.Releases, q, cat == 3040)
		if len(cands) > 0 {
			return laneResult{Lane: l.lane, Searched: true, Candidates: cands}
		}
	}
	if len(issues) > 0 {
		// A lane whose indexers failed did not look; an empty answer from the others is not
		// "searched and found nothing" for this lane (R9).
		return laneUnavailable(l.lane, "%s", strings.Join(issues, "; "))
	}
	return laneResult{Lane: l.lane, Searched: true}
}

// laneIssueText lists the failures that belong to this lane's own indexers.
func laneIssueText(errs, skipped map[string]string, names map[string]bool) []string {
	var out []string
	add := func(m map[string]string, verb string) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if names[k] {
				out = append(out, fmt.Sprintf("%s %s: %s", k, verb, m[k]))
			}
		}
	}
	add(errs, "failed")
	add(skipped, "paused")
	return out
}

func (l *laneIndexed) score(rels []indexer.Release, q laneQuery, losslessCategory bool) []laneCandidate {
	var out []laneCandidate
	for _, r := range rels {
		if r.Transport != l.transport {
			continue
		}
		sc, ok := laneScoreRelease(r, q, losslessCategory)
		if !ok {
			continue
		}
		out = append(out, laneCandidate{
			ReleaseName: r.Title,
			Source:      acqSource{Indexer: r.Indexer, URL: r.DownloadURL, InfoHash: strings.ToLower(r.InfoHash), Size: r.SizeBytes},
			SizeBytes:   r.SizeBytes,
			Score:       l.base + sc,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > laneMaxCandidates {
		out = out[:laneMaxCandidates]
	}
	return out
}

// laneScoreRelease pre-scores a release by its name (music.ParseRelease). It returns false
// for a release that cannot hold the song: lossy, a discography, oversized, by another
// artist, or a torrent with no seeders. Names that state no quality pass only from the
// lossless category (3040); the audio check has the final word either way.
func laneScoreRelease(r indexer.Release, q laneQuery, losslessCategory bool) (int, bool) {
	if r.SizeBytes > laneMaxReleaseBytes {
		return 0, false
	}
	pr := music.ParseRelease(r.Title)
	if pr.Discography {
		return 0, false
	}
	score := 0
	switch {
	case music.IsLosslessQuality(pr.Quality):
		score += 100 + music.QualityRank(pr.Quality)
	case pr.Quality == music.QualityUnknown && losslessCategory:
	default:
		return 0, false
	}
	hay := " " + acqNorm(r.Title) + " "
	for _, w := range strings.Fields(acqNorm(q.Artist)) {
		if !strings.Contains(hay, " "+w+" ") {
			return 0, false
		}
	}
	if a := acqNorm(q.Album); a != "" && strings.Contains(hay, " "+a+" ") {
		score += 200
	}
	if t := acqNorm(q.Title); t != "" && strings.Contains(hay, " "+t+" ") {
		score += 100 // a single, or an album named for the song
	}
	if r.Transport == indexer.TransportTorrent {
		if r.Seeders <= 0 {
			return 0, false
		}
		score += min(r.Seeders, 50)
	}
	return score, true
}

func laneVPNProblem(ctx context.Context, vpn VPNCheck) string {
	if vpn == nil {
		return "VPN attachment not confirmed: nothing recorded at install (KTD20)"
	}
	ok, why := vpn(ctx)
	if ok {
		return ""
	}
	if strings.TrimSpace(why) == "" {
		why = "check did not pass"
	}
	return "VPN attachment not confirmed: " + why
}

// ---- Soulseek lane ----

type laneSoulseek struct {
	client LaneSlskd
	vpn    VPNCheck
}

func (l *laneSoulseek) name() string { return LaneSoulseek }

var laneSoulseekExts = map[string]bool{"flac": true, "wav": true, "ape": true, "wv": true}

func (l *laneSoulseek) search(ctx context.Context, q laneQuery) laneResult {
	if l.client == nil {
		return laneUnavailable(LaneSoulseek, "soulseek lane is not configured")
	}
	if why := laneVPNProblem(ctx, l.vpn); why != "" {
		return laneUnavailable(LaneSoulseek, "%s", why)
	}
	files, err := l.client.Search(ctx, q.text(), slskd.SearchOptions{})
	if err != nil {
		return laneUnavailable(LaneSoulseek, "slskd search failed: %v", err)
	}
	title := acqNorm(q.Title)
	var out []laneCandidate
	for _, f := range files {
		ext := strings.ToLower(strings.TrimPrefix(f.Extension, "."))
		if ext == "" {
			ext = strings.ToLower(strings.TrimPrefix(filepath.Ext(f.Filename), "."))
		}
		if !laneSoulseekExts[ext] {
			continue
		}
		base := f.Filename
		if i := strings.LastIndexAny(base, `\/`); i >= 0 {
			base = base[i+1:]
		}
		base = strings.TrimSuffix(base, filepath.Ext(base))
		if !strings.Contains(" "+acqNorm(base)+" ", " "+title+" ") {
			continue
		}
		if f.BitDepth > 0 && f.BitDepth < 16 {
			continue
		}
		if laneNamesOtherVersion(base, q) {
			continue
		}
		hay := " " + acqNorm(f.Filename) + " "
		artistOK := true
		for _, w := range strings.Fields(acqNorm(q.Artist)) {
			if !strings.Contains(hay, " "+w+" ") {
				artistOK = false
				break
			}
		}
		if !artistOK {
			continue
		}
		sc := 100
		if f.FreeSlot {
			sc += 50
		}
		sc -= min(f.QueueLength, 40)
		if q.LengthMs > 0 && f.LengthSec > 0 {
			if d := f.LengthSec - q.LengthMs/1000; d < -3 || d > 3 {
				sc -= 60 // a different length is likely a different version
			}
		}
		out = append(out, laneCandidate{
			ReleaseName: f.Username + ": " + f.Filename,
			Source:      acqSource{User: f.Username, Filename: f.Filename, Size: f.Size},
			SizeBytes:   f.Size,
			Score:       laneBaseSoulseek + sc,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > laneMaxCandidates {
		out = out[:laneMaxCandidates]
	}
	return laneResult{Lane: LaneSoulseek, Searched: true, Candidates: out}
}

// ---- lane status (R9, R29) ----

// laneRecordStatus stores what the lane pass learned, for the status page. A lane that
// searched is available; one that could not look is unavailable with its reason, and keeps
// the time it first became unavailable.
func laneRecordStatus(ctx context.Context, db *sql.DB, now time.Time, r laneResult, vpnOK bool) error {
	status, reason := "available", ""
	if !r.Searched {
		status, reason = "unavailable", clip(r.Reason, 500)
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO mus_lane_status (lane, status, reason, vpn_ok, checked_at, last_ok_at, unavailable_since)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(lane) DO UPDATE SET status = excluded.status, reason = excluded.reason,
		   vpn_ok = excluded.vpn_ok, checked_at = excluded.checked_at,
		   last_ok_at = CASE WHEN excluded.status = 'available' THEN excluded.checked_at ELSE mus_lane_status.last_ok_at END,
		   unavailable_since = CASE WHEN excluded.status = 'available' THEN 0
		     WHEN mus_lane_status.unavailable_since = 0 THEN excluded.checked_at
		     ELSE mus_lane_status.unavailable_since END`,
		r.Lane, status, reason, boolInt(vpnOK), now.Unix(),
		map[bool]int64{true: now.Unix(), false: 0}[r.Searched],
		map[bool]int64{true: 0, false: now.Unix()}[r.Searched])
	if err != nil {
		return fmt.Errorf("recording %s lane status: %w", r.Lane, err)
	}
	return nil
}

// ---- candidate rows ----

// candRow is a mus_candidates row as the acquirer reads it.
type candRow struct {
	ID            int64
	ItemID        int64
	Lane          string
	State         string
	ReleaseName   string
	Source        acqSource
	ClientJobID   string
	StagingPath   string
	Score         int
	SizeBytes     int64
	ReservedBytes int64
	CreatedAt     int64
}

const candCols = `id, item_id, lane, state, release_name, source_url, client_job_id, staging_path, score, size_bytes, reserved_bytes, created_at`

func scanCand(r rowScanner) (candRow, error) {
	var c candRow
	var src string
	if err := r.Scan(&c.ID, &c.ItemID, &c.Lane, &c.State, &c.ReleaseName, &src, &c.ClientJobID,
		&c.StagingPath, &c.Score, &c.SizeBytes, &c.ReservedBytes, &c.CreatedAt); err != nil {
		return candRow{}, err
	}
	s, err := acqDecodeSource(src)
	if err != nil {
		return candRow{}, err
	}
	c.Source = s
	return c, nil
}

// acqNextFound is the best candidate not yet tried.
func acqNextFound(ctx context.Context, q store.Execer, itemID int64) (*candRow, error) {
	c, err := scanCand(q.QueryRowContext(ctx,
		`SELECT `+candCols+` FROM mus_candidates WHERE item_id = ? AND state = 'found'
		 ORDER BY score DESC, id ASC LIMIT 1`, itemID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading next candidate of item %d: %w", itemID, err)
	}
	return &c, nil
}

// acqActiveCandidate is the candidate being downloaded or checked.
func acqActiveCandidate(ctx context.Context, q store.Execer, itemID int64) (*candRow, error) {
	c, err := scanCand(q.QueryRowContext(ctx,
		`SELECT `+candCols+` FROM mus_candidates WHERE item_id = ? AND state = 'downloading'
		 ORDER BY id DESC LIMIT 1`, itemID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading active candidate of item %d: %w", itemID, err)
	}
	return &c, nil
}

// laneIdentity is what makes two candidates the same release, so a later search does not
// queue what an earlier one already tried.
// It is the lane and the release name only: size_bytes is the indexer's figure and the name
// is what repeats, and a key that could change would let the same release come back as new.
func laneIdentity(lane, name string) string {
	return lane + "|" + acqNorm(name)
}

// laneInsertNew stores the candidates the item has not seen before and returns how many.
func laneInsertNew(ctx context.Context, tx store.Execer, itemID int64, lane string, cands []laneCandidate, now time.Time) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT lane, release_name FROM mus_candidates WHERE item_id = ?`, itemID)
	if err != nil {
		return 0, fmt.Errorf("listing candidates of item %d: %w", itemID, err)
	}
	seen := map[string]bool{}
	for rows.Next() {
		var l, n string
		if err := rows.Scan(&l, &n); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scanning candidate identity: %w", err)
		}
		seen[laneIdentity(l, n)] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	_ = rows.Close()

	n := 0
	for _, c := range cands {
		id := laneIdentity(lane, c.ReleaseName)
		if seen[id] {
			continue
		}
		seen[id] = true
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO mus_candidates (item_id, lane, state, release_name, source_url, score, size_bytes, created_at)
			 VALUES (?, ?, 'found', ?, ?, ?, ?, ?)`,
			itemID, lane, c.ReleaseName, c.Source.encode(), c.Score, c.SizeBytes, now.Unix()); err != nil {
			return n, fmt.Errorf("recording %s candidate %q: %w", lane, clip(c.ReleaseName, 80), err)
		}
		n++
	}
	return n, nil
}

// laneNamesOtherVersion reports whether a file name carries a version word (live, remix, ...)
// that the request does not (R6).
func laneNamesOtherVersion(name string, q laneQuery) bool {
	have := " " + acqNorm(name) + " "
	want := " " + acqNorm(q.VersionHint) + " " + acqNorm(q.Title) + " "
	for _, v := range stagingVersionWords {
		if strings.Contains(have, " "+v+" ") && !strings.Contains(want, " "+v+" ") {
			return true
		}
	}
	return false
}
