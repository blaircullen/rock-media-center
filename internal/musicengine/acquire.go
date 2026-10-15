package musicengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tristenlammi/arrmada/internal/audiocheck"
	"github.com/tristenlammi/arrmada/internal/download"
	"github.com/tristenlammi/arrmada/internal/store"
)

// Decision options the acquirer raises with the "no_genuine_copy" decision (R15). U13
// matches an answer against these strings.
const (
	AcquireOptionLossy = "lossy" // take the official YouTube audio as a labelled lossy file
	AcquireOptionWait  = "wait"  // keep waiting; try the lanes again later
	// DecisionNoGenuineCopy is the mus_decisions kind.
	DecisionNoGenuineCopy = "no_genuine_copy"
)

// Timings. They are variables of the package, not settings: the engine's tick is about a
// minute, and these only say how often a due step is asked to wait.
const (
	acqPollInterval = 30 * time.Second
	acqLaneRetry    = 10 * time.Minute
	acqSpaceRetry   = 15 * time.Minute
	// acqVanishGrace is how long a download may be missing from a complete client queue
	// read before it counts as lost.
	acqVanishGrace = 30 * time.Minute
	// acqBlindTries: a candidate whose check stays BLIND this many times is dropped without
	// calling its audio bad (R14).
	acqBlindTries = 3
	// acqAddTries: an add or fetch that keeps failing this many times rejects the candidate.
	acqAddTries = 3
)

// StagingSpace is the space gate's reservation side (R27, KTD10). U12's gate implements it;
// without one, reservations always succeed. Reserve is asked before a download starts and
// says no when the download could take the volume below the floor or the volume cannot be
// measured. Release is called when the download is checked, rejected or lost. The reserved
// size is also written to mus_candidates.reserved_bytes, so a restarted gate can rebuild its
// holds from the database. It is the same contract as SpaceReserver, which *Space implements;
// the gate reads the item's kind (intake or sweep) itself.
type StagingSpace = SpaceReserver

type acqAllowAllSpace struct{}

func (acqAllowAllSpace) Reserve(context.Context, int64, int64) (bool, string) {
	return true, ""
}
func (acqAllowAllSpace) Release(context.Context, int64) {}

// AcquireConfig wires an Acquire. Staging and Checker are required. A nil lane client makes
// that lane report unavailable ("not configured"), never skipped (R9).
type AcquireConfig struct {
	DB *sql.DB

	Indexer   LaneIndexer
	Downloads LaneDownloader
	Slskd     LaneSlskd
	YouTube   LaneYouTubeClient

	Checker   AudioChecker
	Decisions DecisionRaiser // nil: write mus_decisions rows directly
	Space     StagingSpace   // nil: reservations always succeed
	VPN       VPNCheck       // nil: torrent and Soulseek lanes are unavailable (KTD20)
	// FileTags reads a downloaded file's tags; nil means ffprobe.
	FileTags AcquireTagReader

	Staging *Staging
	// Category is the download-client category; empty means "rmc-music".
	Category string
	// SoulseekDir is slskd's download folder as this process sees it; the lane finds a
	// finished transfer there by file name and moves it into staging.
	SoulseekDir string

	Now func() time.Time
	Log *slog.Logger
}

// Acquire implements Acquirer (U9): it walks the lanes in order, downloads one candidate at
// a time into staging, checks the target track and hands the verified file to placement.
// Every step reads its state from the database and is safe to run twice.
type Acquire struct {
	db        *sql.DB
	lanes     []laneSearcher
	downloads LaneDownloader
	slskd     LaneSlskd
	yt        LaneYouTubeClient
	checker   AudioChecker
	decisions DecisionRaiser
	space     StagingSpace
	staging   *Staging
	category  string
	slskDir   string
	vpn       VPNCheck
	fileTags  AcquireTagReader
	now       func() time.Time
	log       *slog.Logger
}

var _ Acquirer = (*Acquire)(nil)

// NewAcquire builds the acquirer.
func NewAcquire(cfg AcquireConfig) (*Acquire, error) {
	if cfg.DB == nil {
		return nil, errors.New("musicengine acquire: no database")
	}
	if cfg.Staging == nil {
		return nil, errors.New("musicengine acquire: no staging")
	}
	if cfg.Checker == nil {
		return nil, errors.New("musicengine acquire: no audio checker")
	}
	a := &Acquire{
		db: cfg.DB, downloads: cfg.Downloads, slskd: cfg.Slskd, yt: cfg.YouTube,
		checker: cfg.Checker, decisions: cfg.Decisions, space: cfg.Space, staging: cfg.Staging,
		category: cfg.Category, slskDir: cfg.SoulseekDir, vpn: cfg.VPN, now: cfg.Now, log: cfg.Log,
	}
	if a.decisions == nil {
		a.decisions = tableDecisions{}
	}
	if a.space == nil {
		a.space = acqAllowAllSpace{}
	}
	if a.category == "" {
		a.category = acqCategoryDefault
	}
	a.fileTags = cfg.FileTags
	if a.fileTags == nil {
		a.fileTags = ffprobeAcquireTags
	}
	if a.now == nil {
		a.now = time.Now
	}
	if a.log == nil {
		a.log = slog.New(slog.DiscardHandler)
	}
	var usenetIdx, torrentIdx LaneIndexer = cfg.Indexer, cfg.Indexer
	if cfg.Downloads == nil {
		// Without a download client the lane cannot grab what it finds, so it must not
		// claim to have searched.
		usenetIdx, torrentIdx = nil, nil
	}
	a.lanes = []laneSearcher{
		&laneIndexed{lane: LaneUsenet, transport: "usenet", base: laneBaseUsenet, idx: usenetIdx, cats: laneUsenetCategories},
		&laneIndexed{lane: LaneTorrent, transport: "torrent", base: laneBaseTorrent, idx: torrentIdx, vpn: cfg.VPN, cats: laneUsenetCategories},
		&laneSoulseek{client: cfg.Slskd, vpn: cfg.VPN},
	}
	return a, nil
}

// Step runs one item's current state. See Acquirer.
func (a *Acquire) Step(ctx context.Context, m Machine, it Item) error {
	cur, err := getItem(ctx, m.DB(), it.ID)
	if err != nil {
		return err
	}
	switch cur.State {
	case StateSearching, StateLaneUnavailable, StateFailedCheck:
		return a.searchStep(ctx, m, cur)
	case StateDownloading:
		return a.downloadStep(ctx, m, cur)
	case StateChecking:
		return a.checkStep(ctx, m, cur)
	}
	return nil
}

// ---- searching ----

func (a *Acquire) query(ctx context.Context, q store.Execer, it Item) (laneQuery, Recording, error) {
	if it.RecordingID == 0 {
		return laneQuery{}, Recording{}, fmt.Errorf("item %d has no pinned recording to search for", it.ID)
	}
	rec, err := getRecording(ctx, q, it.RecordingID)
	if err != nil {
		return laneQuery{}, Recording{}, err
	}
	lq := laneQuery{Artist: rec.Artist, Title: rec.Title, VersionHint: rec.VersionHint, LengthMs: rec.LengthMs}
	if en, ok, err := entryForItem(ctx, q, it.ID); err != nil {
		return laneQuery{}, Recording{}, err
	} else if ok {
		lq.Album = en.Album
	}
	return lq, rec, nil
}

type acqPass struct {
	unavailable []string // "lane: reason"
	searched    []string
}

// searchStep handles searching, lane_unavailable and failed_check: try the best untried
// candidate; when there is none, search the lanes in order and stop at the first lane that
// yields something new. Nothing new anywhere ends in lane_unavailable (a lane could not
// look, R9) or waiting_blair (every lane looked and nothing passes, R15).
func (a *Acquire) searchStep(ctx context.Context, m Machine, it Item) error {
	lq, rec, err := a.query(ctx, m.DB(), it)
	if err != nil {
		return err
	}
	cand, err := acqNextFound(ctx, m.DB(), it.ID)
	if err != nil {
		return err
	}
	var pass acqPass
	if cand == nil {
		pass, err = a.lanePass(ctx, m, it, lq)
		if err != nil {
			return err
		}
		if cand, err = acqNextFound(ctx, m.DB(), it.ID); err != nil {
			return err
		}
	}
	if cand != nil {
		// A candidate kept from an earlier pass (space wait, restart) was found when the VPN
		// was up; ask again before it starts (finding 5).
		if why := a.vpnBlock(ctx, cand.Lane); why != "" {
			return a.markLaneUnavailable(ctx, m, it, acqPass{unavailable: []string{cand.Lane + ": " + why}})
		}
		return a.startCandidate(ctx, m, it, rec, *cand)
	}
	if len(pass.unavailable) > 0 {
		return a.markLaneUnavailable(ctx, m, it, pass)
	}
	return a.raiseNoGenuine(ctx, m, it, pass)
}

// lanePass searches the lanes in order until one produces a candidate the item has not seen.
func (a *Acquire) lanePass(ctx context.Context, m Machine, it Item, lq laneQuery) (acqPass, error) {
	var pass acqPass
	for _, l := range a.lanes {
		res := l.search(ctx, lq)
		if ctx.Err() != nil {
			return pass, ctx.Err() // shutting down is not a lane outage
		}
		res.Lane = l.name()
		vpnOK := true
		if res.Lane != LaneUsenet {
			ok, _ := a.vpnState(ctx)
			vpnOK = ok
		}
		if err := laneRecordStatus(ctx, m.DB(), m.Now(), res, vpnOK); err != nil {
			a.log.Warn("recording lane status", "lane", res.Lane, "err", err)
		}
		if !res.Searched {
			pass.unavailable = append(pass.unavailable, res.Lane+": "+res.Reason)
			continue
		}
		pass.searched = append(pass.searched, res.Lane)
		n, err := a.insertCandidates(ctx, m, it.ID, res)
		if err != nil {
			return pass, err
		}
		if n > 0 {
			return pass, nil
		}
	}
	return pass, nil
}

// vpnBlock is the reason a torrent or Soulseek operation may not run right now, or "" when
// it may (KTD20). It fails closed: no check, or a failing one, blocks.
func (a *Acquire) vpnBlock(ctx context.Context, lane string) string {
	if lane != LaneTorrent && lane != LaneSoulseek {
		return ""
	}
	return laneVPNProblem(ctx, a.vpn)
}

// vpnHold stops a download step that the VPN does not allow. A candidate with no client job
// yet goes back to 'found' and the item to lane_unavailable, so nothing is enqueued; a
// running job is left alone and its poll is deferred.
func (a *Acquire) vpnHold(ctx context.Context, m Machine, it Item, c candRow, why string) error {
	reason := "lane unavailable: " + c.Lane + ": " + clip(why, 300)
	if c.ClientJobID != "" {
		return a.acqDefer(ctx, m, it, acqLaneRetry, reason)
	}
	err := store.WithTx(ctx, m.DB(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE mus_candidates SET state = 'found', reserved_bytes = 0 WHERE id = ? AND state = 'downloading'`, c.ID); err != nil {
			return fmt.Errorf("returning candidate %d to the queue: %w", c.ID, err)
		}
		if err := m.TransitionTx(ctx, tx, it.ID, StateSearching, "VPN not confirmed; candidate kept for later"); err != nil {
			return err
		}
		if err := m.TransitionTx(ctx, tx, it.ID, StateLaneUnavailable, reason); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE mus_items SET next_attempt_at = ? WHERE id = ?`, m.Now().Add(acqLaneRetry).Unix(), it.ID)
		return err
	})
	a.space.Release(ctx, it.ID)
	return err
}

func (a *Acquire) vpnState(ctx context.Context) (bool, string) {
	if a.vpn == nil {
		return false, "not recorded"
	}
	return a.vpn(ctx)
}

func (a *Acquire) insertCandidates(ctx context.Context, m Machine, itemID int64, res laneResult) (int, error) {
	var n int
	err := store.WithTx(ctx, m.DB(), func(tx *sql.Tx) error {
		var err error
		n, err = laneInsertNew(ctx, tx, itemID, res.Lane, res.Candidates, m.Now())
		return err
	})
	return n, err
}

func (a *Acquire) hasYouTubeApproval(ctx context.Context, q store.Execer, itemID int64) (bool, error) {
	var n int
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mus_candidates WHERE item_id = ? AND lane = ?`, itemID, LaneYouTube).Scan(&n); err != nil {
		return false, fmt.Errorf("checking lossy approval of item %d: %w", itemID, err)
	}
	return n > 0, nil
}

// toSearching moves an item that is in failed_check or lane_unavailable to searching inside
// tx, so the next move (downloading, lane_unavailable, waiting_blair) is a legal edge.
func (a *Acquire) toSearching(ctx context.Context, tx store.Execer, m Machine, it Item, reason string) error {
	if it.State == StateFailedCheck || it.State == StateLaneUnavailable {
		return m.TransitionTx(ctx, tx, it.ID, StateSearching, reason)
	}
	return nil
}

func (a *Acquire) markLaneUnavailable(ctx context.Context, m Machine, it Item, pass acqPass) error {
	reason := "lane unavailable: " + clip(strings.Join(pass.unavailable, "; "), 400)
	retry := m.Now().Add(acqLaneRetry).Unix()
	return store.WithTx(ctx, m.DB(), func(tx *sql.Tx) error {
		if it.State == StateLaneUnavailable {
			_, err := tx.ExecContext(ctx,
				`UPDATE mus_items SET last_reason = ?, next_attempt_at = ?, updated_at = ? WHERE id = ? AND state = ?`,
				reason, retry, m.Now().Unix(), it.ID, string(StateLaneUnavailable))
			return err
		}
		if err := a.toSearching(ctx, tx, m, it, "searching again"); err != nil {
			return err
		}
		if err := m.TransitionTx(ctx, tx, it.ID, StateLaneUnavailable, reason); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE mus_items SET next_attempt_at = ? WHERE id = ?`, retry, it.ID)
		return err
	})
}

// raiseNoGenuine sends the item to Blair with lossy / wait / drop (R15). Only reached when
// every lane really searched.
func (a *Acquire) raiseNoGenuine(ctx context.Context, m Machine, it Item, pass acqPass) error {
	var rejected int
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mus_candidates WHERE item_id = ? AND state = 'rejected'`, it.ID).Scan(&rejected); err != nil {
		return fmt.Errorf("counting rejected candidates of item %d: %w", it.ID, err)
	}
	reason := "no genuine lossless copy: every lane searched (" + strings.Join(pass.searched, ", ") + ") and found nothing new"
	if rejected > 0 {
		reason = fmt.Sprintf("no genuine lossless copy: %d candidate(s) failed the checks and no lane has another", rejected)
	}
	options := []string{AcquireOptionLossy, AcquireOptionWait, OptionDrop}
	if has, err := a.hasYouTubeApproval(ctx, m.DB(), it.ID); err != nil {
		return err
	} else if has {
		options = []string{AcquireOptionWait, OptionDrop} // the lossy source was already tried
		reason += "; the official YouTube audio was tried too"
	}
	return store.WithTx(ctx, m.DB(), func(tx *sql.Tx) error {
		if err := a.toSearching(ctx, tx, m, it, "searching again"); err != nil {
			return err
		}
		if err := a.decisions.Raise(ctx, tx, DecisionRequest{
			ItemID: it.ID, Kind: DecisionNoGenuineCopy, Reason: reason, Options: options, Now: m.Now(),
		}); err != nil {
			return err
		}
		return m.TransitionTx(ctx, tx, it.ID, StateWaitingBlair, reason)
	})
}

// AcceptLossy is what U13 calls when Blair answers "lossy" (R15): it queues the official
// YouTube audio as the item's next candidate and sends the item back to searching. The
// file stays in its own lossy container and is labelled as lossy; nothing wraps it in FLAC.
func (a *Acquire) AcceptLossy(ctx context.Context, m Machine, itemID int64) error {
	if a.yt == nil {
		return errors.New("musicengine: the YouTube audio lane is not configured")
	}
	return store.WithTx(ctx, m.DB(), func(tx *sql.Tx) error {
		it, err := getItem(ctx, tx, itemID)
		if err != nil {
			return err
		}
		if it.State != StateWaitingBlair {
			return fmt.Errorf("%w: item %d is %s, not waiting for Blair", ErrInvalidTransition, itemID, it.State)
		}
		if has, err := a.hasYouTubeApproval(ctx, tx, itemID); err != nil {
			return err
		} else if has {
			return fmt.Errorf("musicengine: item %d already has a YouTube candidate", itemID)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO mus_candidates (item_id, lane, state, release_name, source_url, score, created_at)
			 VALUES (?, ?, 'found', 'official YouTube audio (lossy, approved)', '{}', 0, ?)`,
			itemID, LaneYouTube, m.Now().Unix()); err != nil {
			return fmt.Errorf("queueing YouTube candidate for item %d: %w", itemID, err)
		}
		return m.TransitionTx(ctx, tx, itemID, StateSearching, "Blair approved a lossy copy from official YouTube audio")
	})
}

// ---- starting a download ----

// acqReserveBytes is the room a candidate needs: usenet unpacks beside the download, so it
// holds twice its size; an unknown size reserves 1 GiB (128 MiB for YouTube audio).
func acqReserveBytes(c candRow) int64 {
	size := c.SizeBytes
	if size <= 0 {
		if c.Lane == LaneYouTube {
			return 128 << 20
		}
		return 1 << 30
	}
	if c.Lane == LaneUsenet {
		size *= 2
	}
	return size
}

func (a *Acquire) startCandidate(ctx context.Context, m Machine, it Item, rec Recording, c candRow) error {
	need := acqReserveBytes(c)
	if ok, why := a.space.Reserve(ctx, it.ID, need); !ok {
		reason := "waiting for space: " + clip(why, 300)
		_, err := m.DB().ExecContext(ctx,
			`UPDATE mus_items SET last_reason = ?, next_attempt_at = ?, updated_at = ? WHERE id = ? AND state = ?`,
			reason, m.Now().Add(acqSpaceRetry).Unix(), m.Now().Unix(), it.ID, string(it.State))
		if err != nil {
			return fmt.Errorf("recording space wait on item %d: %w", it.ID, err)
		}
		return nil
	}
	dir, err := a.staging.ensureCandidateDir(it.ID, c.ID)
	if err != nil {
		a.space.Release(ctx, it.ID)
		return err
	}
	err = store.WithTx(ctx, m.DB(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE mus_candidates SET state = 'downloading', reserved_bytes = ?, staging_path = ?
			 WHERE id = ? AND state = 'found'`, need, dir, c.ID)
		if err != nil {
			return fmt.Errorf("starting candidate %d: %w", c.ID, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("candidate %d is no longer waiting to be tried", c.ID)
		}
		if err := a.toSearching(ctx, tx, m, it, "next candidate"); err != nil {
			return err
		}
		return m.TransitionTx(ctx, tx, it.ID, StateDownloading, fmt.Sprintf("downloading from %s: %s", c.Lane, clip(c.ReleaseName, 160)))
	})
	if err != nil {
		a.space.Release(ctx, it.ID)
		return err
	}
	cur, err := getItem(ctx, m.DB(), it.ID)
	if err != nil {
		return err
	}
	return a.downloadStep(ctx, m, cur)
}

// ---- downloading ----

func (a *Acquire) downloadStep(ctx context.Context, m Machine, it Item) error {
	c, err := acqActiveCandidate(ctx, m.DB(), it.ID)
	if err != nil {
		return err
	}
	if c == nil {
		return m.Transition(ctx, it.ID, StateSearching, "no candidate was downloading; searching again")
	}
	if why := a.vpnBlock(ctx, c.Lane); why != "" {
		return a.vpnHold(ctx, m, it, *c, why)
	}
	dir, err := a.staging.ensureCandidateDir(it.ID, c.ID)
	if err != nil {
		return err
	}
	switch c.Lane {
	case LaneYouTube:
		return a.youtubeDownload(ctx, m, it, *c, dir)
	case LaneSoulseek:
		return a.soulseekDownload(ctx, m, it, *c, dir)
	default:
		return a.clientDownload(ctx, m, it, *c, dir)
	}
}

// acqDefer asks the engine to call the step again after d, without changing state.
func (a *Acquire) acqDefer(ctx context.Context, m Machine, it Item, d time.Duration, reason string) error {
	_, err := m.DB().ExecContext(ctx,
		`UPDATE mus_items SET next_attempt_at = ?, last_reason = CASE WHEN ? <> '' THEN ? ELSE last_reason END, updated_at = ?
		 WHERE id = ? AND state = ?`,
		m.Now().Add(d).Unix(), reason, reason, m.Now().Unix(), it.ID, string(it.State))
	if err != nil {
		return fmt.Errorf("deferring item %d: %w", it.ID, err)
	}
	return nil
}

func (a *Acquire) clientDownload(ctx context.Context, m Machine, it Item, c candRow, dir string) error {
	if a.downloads == nil {
		return errors.New("no download client service is configured")
	}
	if c.ClientJobID == "" {
		req := download.AddRequest{
			Name: c.ReleaseName, Category: a.category, SavePath: dir,
			Transport: download.TransportUsenet,
		}
		if c.Lane == LaneTorrent {
			req.Transport = download.TransportTorrent
		}
		key, err := a.addToClient(ctx, c, &req)
		if err != nil {
			if it.Attempts+1 >= acqAddTries && ctx.Err() == nil {
				return a.rejectDownload(ctx, m, it, c, "could not start the download: "+err.Error())
			}
			return fmt.Errorf("starting %s download of %q: %w", c.Lane, c.ReleaseName, err)
		}
		if key == "" {
			key = "name:" + c.ReleaseName // the client gave no key; the queue is matched by name
		}
		if _, err := m.DB().ExecContext(ctx, `UPDATE mus_candidates SET client_job_id = ? WHERE id = ?`, key, c.ID); err != nil {
			return fmt.Errorf("recording client key of candidate %d: %w", c.ID, err)
		}
		c.ClientJobID = key
	}
	items, complete, err := a.downloads.QueueComplete(ctx)
	if err != nil {
		return fmt.Errorf("reading the download queue: %w", err)
	}
	qi, found := acqMatchQueue(items, c)
	if !found {
		if complete && m.Now().Sub(time.Unix(it.EnteredAt, 0)) > acqVanishGrace {
			return a.rejectDownload(ctx, m, it, c, "the download is no longer in the client")
		}
		return a.acqDefer(ctx, m, it, acqPollInterval, "")
	}
	if qi.Phase() == "error" || qi.State == "error" {
		why := qi.FailMessage
		if why == "" {
			why = "client state " + qi.RawState
		}
		return a.rejectDownload(ctx, m, it, c, "download failed: "+why)
	}
	if !qi.Complete() {
		return a.acqDefer(ctx, m, it, acqPollInterval, "")
	}
	if qi.ContentPath == "" {
		return a.rejectDownload(ctx, m, it, c, "the client finished but reported no file location")
	}
	if err := a.adopt(ctx, m, c, qi.ContentPath, dir); err != nil {
		return fmt.Errorf("moving the finished download into staging: %w", err)
	}
	return a.toChecking(ctx, m, it, c, dir)
}

// addToClient fetches the release (NZB bytes or magnet) and hands it to the client.
func (a *Acquire) addToClient(ctx context.Context, c candRow, req *download.AddRequest) (string, error) {
	if idx := a.fetcher(); idx != nil && c.Source.Indexer != "" {
		fr, err := idx.Fetch(ctx, c.Source.Indexer, c.Source.URL)
		if err != nil {
			return "", fmt.Errorf("fetching from %s: %w", c.Source.Indexer, err)
		}
		req.File, req.Filename, req.URL = fr.File, fr.Filename, fr.URL
	} else {
		req.URL = c.Source.URL
	}
	if len(req.File) == 0 && req.URL == "" {
		return "", errors.New("the indexer gave neither a file nor a link")
	}
	added, err := a.downloads.AddDownload(ctx, *req)
	if err != nil {
		return "", err
	}
	key := added.Key
	if key == "" && c.Source.InfoHash != "" {
		key = c.Source.InfoHash
	}
	return key, nil
}

// fetcher is the indexer service, reached through the usenet lane (both indexed lanes share it).
func (a *Acquire) fetcher() LaneIndexer {
	for _, l := range a.lanes {
		if li, ok := l.(*laneIndexed); ok && li.idx != nil {
			return li.idx
		}
	}
	return nil
}

func acqMatchQueue(items []download.Item, c candRow) (download.Item, bool) {
	for _, qi := range items {
		if name, ok := strings.CutPrefix(c.ClientJobID, "name:"); ok {
			if strings.EqualFold(qi.Name, name) {
				return qi, true
			}
			continue
		}
		if strings.EqualFold(qi.Hash, c.ClientJobID) {
			return qi, true
		}
	}
	return download.Item{}, false
}

// acqAlreadyAdopted: a previous attempt recorded where it was moving the finished download
// (staging_path set to the destination before the rename, finding 8) and the destination is
// there, so a crash after the rename left the work done.
func acqAlreadyAdopted(c candRow, dir string) bool {
	p := filepath.Clean(c.StagingPath)
	if c.StagingPath == "" || p == filepath.Clean(dir) || !stagingInside(dir, p) {
		return false
	}
	_, err := os.Lstat(p)
	return err == nil
}

// adopt moves a finished download into the candidate's staging directory so a crash at any
// point can be resumed: the destination is written to the candidate first, in its own
// statement, then the rename runs. On a retry a destination that exists while the source does
// not is taken as adopted.
func (a *Acquire) adopt(ctx context.Context, m Machine, c candRow, src, dir string) error {
	src, dir = filepath.Clean(src), filepath.Clean(dir)
	if acqAlreadyAdopted(c, dir) {
		// dst only ever appears whole (a rename, or a copy renamed into place), so an existing
		// dst is done work even when the client's original is still there (a copy).
		return nil
	}
	dst := filepath.Join(dir, filepath.Base(src))
	if stagingInside(dir, src) {
		dst = src
	}
	if err := store.WithTx(ctx, m.DB(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE mus_candidates SET staging_path = ? WHERE id = ?`, dst, c.ID)
		return err
	}); err != nil {
		return fmt.Errorf("recording adoption of candidate %d: %w", c.ID, err)
	}
	_, copied, err := acqAdopt(src, dir)
	if err != nil {
		if _, serr := os.Lstat(src); errors.Is(serr, fs.ErrNotExist) {
			if _, derr := os.Lstat(dst); derr == nil {
				return nil // moved by an earlier attempt
			}
		}
		return err
	}
	if copied {
		a.log.Info("musicengine: copied a finished download into staging; the client's original is left in place",
			"candidate", c.ID, "source", src)
	}
	return nil
}

// acqAdopt makes src part of dir: already inside it, or renamed into it. The deployment keeps
// the clients' completed folders on the same volume as staging (U16), so it is normally a
// cheap rename. When the rename is refused because this process may not write the client's
// folder (on Beast slskd runs as root and leaves root-owned 755 folders) or the folders are on
// different filesystems, the file is copied instead: to a temporary name in dir, synced,
// size-checked, then renamed into place, so dst exists only once it is whole. The client's
// original is then left where it is (copied reports true) for the client's own clean-up.
func acqAdopt(src, dir string) (dst string, copied bool, err error) {
	src, dir = filepath.Clean(src), filepath.Clean(dir)
	if stagingInside(dir, src) {
		return src, false, nil
	}
	dst = filepath.Join(dir, filepath.Base(src))
	rerr := os.Rename(src, dst)
	if rerr == nil {
		return dst, false, nil
	}
	if !errors.Is(rerr, fs.ErrPermission) && !errors.Is(rerr, syscall.EXDEV) {
		return "", false, fmt.Errorf("renaming %s to %s: %w", src, dst, rerr)
	}
	if err := acqCopyWhole(src, dst); err != nil {
		return "", false, fmt.Errorf("renaming %s to %s failed (%v), and copying it failed: %w", src, dst, rerr, err)
	}
	return dst, true, nil
}

// acqCopyWhole copies src to dst through a temporary file beside dst, so a crash never leaves
// a partial file under dst's name.
func acqCopyWhole(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	tmp := dst + ".adopting"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	n, err := io.Copy(out, in)
	if err == nil && n != info.Size() {
		err = fmt.Errorf("copied %d of %d bytes", n, info.Size())
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func (a *Acquire) soulseekDownload(ctx context.Context, m Machine, it Item, c candRow, dir string) error {
	if a.slskd == nil {
		return errors.New("slskd is not configured")
	}
	if c.ClientJobID == "" {
		if err := a.slskd.Enqueue(ctx, c.Source.User, c.Source.Filename, c.Source.Size); err != nil {
			if it.Attempts+1 >= acqAddTries && ctx.Err() == nil {
				return a.rejectDownload(ctx, m, it, c, "could not queue the Soulseek download: "+err.Error())
			}
			return fmt.Errorf("queueing Soulseek download from %s: %w", c.Source.User, err)
		}
		if _, err := m.DB().ExecContext(ctx, `UPDATE mus_candidates SET client_job_id = 'slskd' WHERE id = ?`, c.ID); err != nil {
			return fmt.Errorf("recording slskd job of candidate %d: %w", c.ID, err)
		}
	}
	tr, err := a.slskd.GetDownload(ctx, c.Source.User, c.Source.Filename)
	if err != nil {
		if m.Now().Sub(time.Unix(it.EnteredAt, 0)) > acqVanishGrace {
			return a.rejectDownload(ctx, m, it, c, "the Soulseek transfer is gone: "+err.Error())
		}
		return fmt.Errorf("reading the Soulseek transfer: %w", err)
	}
	switch tr.Phase {
	case "failed":
		return a.rejectDownload(ctx, m, it, c, strings.TrimSpace("Soulseek transfer failed: "+tr.State+" "+tr.Exception))
	case "succeeded":
	default:
		return a.acqDefer(ctx, m, it, acqPollInterval, "")
	}
	if adopted := acqAlreadyAdopted(c, dir); !adopted {
		src, err := acqFindSoulseekFile(a.slskDir, c.Source.Filename, c.Source.Size)
		if err != nil {
			return err
		}
		if err := a.adopt(ctx, m, c, src, dir); err != nil {
			return fmt.Errorf("moving the Soulseek file into staging: %w", err)
		}
	}
	return a.toChecking(ctx, m, it, c, dir)
}

// acqSlskdNameMatches: the saved file has the remote file's name, or, when a file of that name
// was already in the folder, slskd's collision name "<stem>_<digits><ext>" (seen on Beast
// 2026-10-15: "... Drunk on a Plane_639276573952246895.flac" beside an older download). The
// size check in the caller then tells the two apart.
func acqSlskdNameMatches(name, base string) bool {
	if strings.EqualFold(name, base) {
		return true
	}
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if len(name) <= len(stem)+1+len(ext) || !strings.EqualFold(filepath.Ext(name), ext) {
		return false
	}
	head, tail := name[:len(stem)+1], strings.TrimSuffix(name[len(stem)+1:], filepath.Ext(name))
	if !strings.EqualFold(head, stem+"_") || tail == "" {
		return false
	}
	for _, r := range tail {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// acqFindSoulseekFile finds THIS transfer's finished file in slskd's download folder. slskd
// saves a transfer under the remote file's own folder name, so the match needs the file name,
// that parent folder and the size together; the same name and size from another peer lands in
// a differently named folder or is a second match. More than one match is ambiguous and is an
// error, never "the newest one" (finding 4).
func acqFindSoulseekFile(root, remote string, size int64) (string, error) {
	if root == "" {
		return "", errors.New("the slskd download folder is not configured")
	}
	parts := strings.FieldsFunc(remote, func(r rune) bool { return r == '\\' || r == '/' })
	if len(parts) == 0 {
		return "", fmt.Errorf("slskd file name %q has no name", remote)
	}
	base := parts[len(parts)-1]
	parent := ""
	if len(parts) > 1 {
		parent = parts[len(parts)-2]
	}
	var matches []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !acqSlskdNameMatches(d.Name(), base) {
			return nil
		}
		if parent != "" && !strings.EqualFold(filepath.Base(filepath.Dir(p)), parent) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if size > 0 && info.Size() != size {
			return nil
		}
		matches = append(matches, p)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("looking for %q in %s: %w", base, root, err)
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("slskd reports %q finished but it is not in %s (folder %q)", base, root, parent)
	case 1:
		return matches[0], nil
	}
	return "", fmt.Errorf("slskd transfer %q matches %d files in %s; cannot tell which is this transfer", base, len(matches), root)
}

func (a *Acquire) youtubeDownload(ctx context.Context, m Machine, it Item, c candRow, dir string) error {
	if a.yt == nil {
		return errors.New("the YouTube audio lane is not configured")
	}
	lq, _, err := a.query(ctx, m.DB(), it)
	if err != nil {
		return err
	}
	src := c.Source
	if src.URL == "" {
		hits, err := a.yt.Search(ctx, lq.Artist, lq.Title)
		if err != nil {
			if it.Attempts+1 >= acqAddTries && ctx.Err() == nil {
				return a.rejectDownload(ctx, m, it, c, "YouTube search failed: "+err.Error())
			}
			return fmt.Errorf("searching YouTube audio: %w", err)
		}
		for _, h := range hits {
			if lq.LengthMs > 0 && h.DurationSec > 0 {
				if d := h.DurationSec - float64(lq.LengthMs)/1000; d < -15 || d > 15 {
					continue // a different length is a different version
				}
			}
			src.URL = h.URL
			break
		}
		if src.URL == "" {
			return a.rejectDownload(ctx, m, it, c, "no official audio upload of the right length on YouTube")
		}
		if _, err := m.DB().ExecContext(ctx, `UPDATE mus_candidates SET source_url = ? WHERE id = ?`, src.encode(), c.ID); err != nil {
			return fmt.Errorf("recording YouTube source of candidate %d: %w", c.ID, err)
		}
	}
	res, err := a.yt.Download(ctx, src.URL, dir)
	if err != nil {
		if it.Attempts+1 >= acqAddTries && ctx.Err() == nil {
			return a.rejectDownload(ctx, m, it, c, "YouTube download failed: "+err.Error())
		}
		return fmt.Errorf("downloading YouTube audio: %w", err)
	}
	if _, err := m.DB().ExecContext(ctx,
		`UPDATE mus_candidates SET client_job_id = 'yt-dlp', staging_path = ?, codec = ?, container = ?, bitrate = ? WHERE id = ?`,
		res.Path, res.Codec, res.Container, res.BitrateBPS/1000, c.ID); err != nil {
		return fmt.Errorf("recording YouTube file of candidate %d: %w", c.ID, err)
	}
	return a.toChecking(ctx, m, it, c, dir)
}

// toChecking moves a finished download on to the check and runs it at once.
func (a *Acquire) toChecking(ctx context.Context, m Machine, it Item, c candRow, dir string) error {
	if c.Lane != LaneYouTube {
		if _, err := m.DB().ExecContext(ctx, `UPDATE mus_candidates SET staging_path = ? WHERE id = ?`, dir, c.ID); err != nil {
			return fmt.Errorf("recording staging path of candidate %d: %w", c.ID, err)
		}
	}
	if err := m.Transition(ctx, it.ID, StateChecking, "downloaded; checking "+clip(c.ReleaseName, 120)); err != nil {
		return err
	}
	cur, err := getItem(ctx, m.DB(), it.ID)
	if err != nil {
		return err
	}
	return a.checkStep(ctx, m, cur)
}

// rejectDownload ends a candidate whose download failed (not whose audio failed) and goes
// back to searching for the next one.
func (a *Acquire) rejectDownload(ctx context.Context, m Machine, it Item, c candRow, reason string) error {
	err := store.WithTx(ctx, m.DB(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE mus_candidates SET state = 'rejected', reject_reason = ?, reserved_bytes = 0, checked_at = ?, expires_at = ?
			 WHERE id = ?`, clip(reason, 500), m.Now().Unix(), m.Now().Add(StagingExpiry).Unix(), c.ID); err != nil {
			return fmt.Errorf("rejecting candidate %d: %w", c.ID, err)
		}
		return m.TransitionTx(ctx, tx, it.ID, StateSearching, "candidate failed: "+clip(reason, 200))
	})
	a.space.Release(ctx, it.ID)
	if err != nil {
		return err
	}
	a.forgetClientJob(ctx, c.Lane, c.ClientJobID)
	return a.chainSearch(ctx, m, it.ID)
}

func (a *Acquire) chainSearch(ctx context.Context, m Machine, itemID int64) error {
	cur, err := getItem(ctx, m.DB(), itemID)
	if err != nil {
		return err
	}
	return a.searchStep(ctx, m, cur)
}

// forgetClientJob asks the client to drop a finished or failed job (files stay; staging
// removes them). A failure is logged: the job is already out of the item's hands.
func (a *Acquire) forgetClientJob(ctx context.Context, lane, key string) {
	if a.downloads == nil || key == "" || strings.HasPrefix(key, "name:") || (lane != LaneUsenet && lane != LaneTorrent) {
		return
	}
	if err := a.downloads.Remove(ctx, key, false); err != nil {
		a.log.Warn("removing download from client", "lane", lane, "key", key, "err", err)
	}
}

// ---- checking ----

func (a *Acquire) checkStep(ctx context.Context, m Machine, it Item) error {
	c, err := acqActiveCandidate(ctx, m.DB(), it.ID)
	if err != nil {
		return err
	}
	if c == nil {
		return m.Transition(ctx, it.ID, StateFailedCheck, "no candidate to check")
	}
	_, rec, err := a.query(ctx, m.DB(), it)
	if err != nil {
		return err
	}
	target := c.StagingPath
	info, statErr := os.Stat(target)
	switch {
	case statErr != nil:
		return a.rejectChecked(ctx, m, it, *c, "", "staged files are gone: "+statErr.Error(), nil, "")
	case info.IsDir():
		target, err = stagingFindTrack(target, rec.Title, rec.VersionHint)
		if err != nil {
			return a.rejectChecked(ctx, m, it, *c, "", err.Error(), nil, "")
		}
	}
	tags, tagsErr := a.fileTags(ctx, target)
	if tagsErr != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if tagsErr != nil {
		a.log.Warn("reading tags of downloaded file", "path", target, "err", tagsErr)
	} else if err := acqSetReleaseIDs(ctx, m.DB(), c.ID, tags); err != nil {
		return err
	}
	res := a.checker.Check(ctx, target)
	lossy := c.Lane == LaneYouTube
	switch {
	case res.Verdict == audiocheck.Blind:
		// Unknown, not bad (R14): look again; only a candidate that stays unreadable is dropped.
		if it.Attempts+1 >= acqBlindTries {
			return a.rejectChecked(ctx, m, it, *c, string(audiocheck.Blind),
				fmt.Sprintf("the check could not read the file %d times: %s", acqBlindTries, res.Reason), &res, target)
		}
		return fmt.Errorf("check of %s was blind: %s", filepath.Base(target), res.Reason)
	case lossy && res.Integrity.OK && res.Verdict != audiocheck.Corrupt:
		// Plan R15: Blair approved this lossy copy; it keeps its lossy container and label.
		return a.acceptChecked(ctx, m, it, *c, "LOSSY", res, target, "")
	case !lossy && res.Verdict == audiocheck.Genuine && res.Integrity.OK:
		// A spectral "Genuine" says nothing about the codec: AAC and MP3 can read as genuine.
		if !isLossless(res.Facts.Codec) {
			return a.rejectChecked(ctx, m, it, *c, string(res.Verdict),
				fmt.Sprintf("not a lossless codec (%q): only FLAC, ALAC, WAV and similar are accepted", res.Facts.Codec), &res, target)
		}
		reject, note := acqCheckIdentity(rec, *c, target, tags, tagsErr, res.Facts.DurationSec)
		if reject != "" {
			return a.rejectChecked(ctx, m, it, *c, string(res.Verdict), reject, &res, target)
		}
		return a.acceptChecked(ctx, m, it, *c, string(res.Verdict), res, target, note)
	}
	reason := fmt.Sprintf("%s: %s", res.Verdict, res.Reason)
	if res.Verdict == audiocheck.Genuine && !res.Integrity.OK {
		reason = "failed the full-file integrity check: " + res.Integrity.Detail
	}
	return a.rejectChecked(ctx, m, it, *c, string(res.Verdict), reason, &res, target)
}

func acqApplyResult(ctx context.Context, tx store.Execer, c candRow, state, verdict, reject string, res *audiocheck.Result, target string, now time.Time) error {
	expires := now.Add(StagingExpiry).Unix()
	if res == nil {
		_, err := tx.ExecContext(ctx,
			`UPDATE mus_candidates SET state = ?, verdict = ?, reject_reason = ?, reserved_bytes = 0, checked_at = ?, expires_at = ? WHERE id = ?`,
			state, verdict, clip(reject, 500), now.Unix(), expires, c.ID)
		if err != nil {
			return fmt.Errorf("recording check of candidate %d: %w", c.ID, err)
		}
		return nil
	}
	f := res.Facts
	var top int64
	var floor any
	if res.Signature != nil {
		top, floor = int64(res.Signature.TopHz), res.Signature.FloorDB
	}
	path := c.StagingPath
	if target != "" {
		path = target
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE mus_candidates SET state = ?, verdict = ?, integrity_ok = ?, reject_reason = ?,
		   codec = ?, container = ?, bit_depth = ?, sample_rate = ?, channels = ?, bitrate = ?,
		   duration_ms = ?, decoded_ms = ?, loudness_lufs = ?, spectral_top_hz = ?, spectral_floor_db = ?,
		   pcm_fingerprint = ?,
		   staging_path = ?, reserved_bytes = 0, checked_at = ?, expires_at = ?
		 WHERE id = ?`,
		state, verdict, boolInt(res.Integrity.OK), clip(reject, 500),
		f.Codec, f.Container, f.BitDepth, f.SampleRate, f.Channels, int(f.BitrateKbps),
		int64(f.DurationSec*1000), int64(res.Integrity.DecodedSec*1000), res.LoudnessLUFS, top, floor,
		res.Fingerprint, path, now.Unix(), expires, c.ID)
	if err != nil {
		return fmt.Errorf("recording check of candidate %d: %w", c.ID, err)
	}
	return nil
}

// acceptChecked marks the candidate chosen and hands the item to placement (a new track) or
// the swap (an upgrade). Its staging_path from here on is the target track file; the rest of
// the release stays beside it until the 7-day expiry (R10).
func (a *Acquire) acceptChecked(ctx context.Context, m Machine, it Item, c candRow, verdict string, res audiocheck.Result, target, note string) error {
	next := StatePlacing
	if it.Kind != KindRequest {
		next = StateSwapping
	}
	err := store.WithTx(ctx, m.DB(), func(tx *sql.Tx) error {
		if err := acqApplyResult(ctx, tx, c, "chosen", verdict, "", &res, target, m.Now()); err != nil {
			return err
		}
		return m.TransitionTx(ctx, tx, it.ID, next, acqAcceptReason(verdict, c, note))
	})
	a.space.Release(ctx, it.ID)
	return err
}

func acqAcceptReason(verdict string, c candRow, note string) string {
	r := fmt.Sprintf("verified %s from %s: %s", verdict, c.Lane, clip(c.ReleaseName, 120))
	if note != "" {
		r += " (" + note + ")"
	}
	return r
}

// acqSetReleaseIDs stores the release and release group ids the downloaded file's own tags
// carry. They are never taken from the requested recording: a file without those tags leaves
// the columns empty (finding 11).
func acqSetReleaseIDs(ctx context.Context, q store.Execer, candID int64, t AcquireFileTags) error {
	if _, err := q.ExecContext(ctx,
		`UPDATE mus_candidates SET release_mbid = ?, release_group_mbid = ? WHERE id = ?`,
		t.ReleaseMBID, t.ReleaseGroupMBID, candID); err != nil {
		return fmt.Errorf("recording release ids of candidate %d: %w", candID, err)
	}
	return nil
}

// rejectChecked records the verdict on the candidate (it stays in staging until its expiry)
// and sends the item to failed_check; the next candidate is then tried.
func (a *Acquire) rejectChecked(ctx context.Context, m Machine, it Item, c candRow, verdict, reason string, res *audiocheck.Result, target string) error {
	err := store.WithTx(ctx, m.DB(), func(tx *sql.Tx) error {
		if err := acqApplyResult(ctx, tx, c, "rejected", verdict, reason, res, target, m.Now()); err != nil {
			return err
		}
		return m.TransitionTx(ctx, tx, it.ID, StateFailedCheck, "candidate rejected: "+clip(reason, 200))
	})
	a.space.Release(ctx, it.ID)
	if err != nil {
		return err
	}
	a.forgetClientJob(ctx, c.Lane, c.ClientJobID)
	return a.chainSearch(ctx, m, it.ID)
}
