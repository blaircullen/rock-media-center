package musicengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tristenlammi/arrmada/internal/audiocheck"
)

// "Get the full album" (R11). The release the placed single came from is pinned. If its
// files are still in staging, the other tracks go through the same checks and the same
// journal as the single. If staging has expired, nothing is placed until Blair has seen the
// release that would be fetched instead (a decision of kind "album_substitute"), and the
// caller supplies the new staging dir once it is fetched. The placed single is kept, and the
// album's copy of that track is discarded unless it is strictly better.

// DecisionAlbumSubstitute is the decision kind raised before an album is placed from a
// release that is not the one in staging. U12/U13 route its answer to ConfirmAlbum.
const DecisionAlbumSubstitute = "album_substitute"

// ErrAlbumUnavailable means the single has no release to build an album from.
var ErrAlbumUnavailable = errors.New("musicengine: no release is recorded for this song, so there is no album to get")

// AlbumRelease is what Blair is shown: exactly which release "the full album" means.
type AlbumRelease struct {
	ReleaseMBID string
	Artist      string
	Title       string
	Edition     string // status, country and date, as MusicBrainz gives them
	TrackCount  int
}

func (r AlbumRelease) String() string {
	s := fmt.Sprintf("%s - %s", r.Artist, r.Title)
	if r.Edition != "" {
		s += " (" + r.Edition + ")"
	}
	return fmt.Sprintf("%s, %d tracks", s, r.TrackCount)
}

// AlbumConfig wires an Album.
type AlbumConfig struct {
	Placement *Placement
	Releases  ReleaseLookup
	Staging   *Staging
	// Substitute, when set, names the release that would be fetched if the pinned one is no
	// longer in staging. Nil means the pinned release itself is fetched again.
	Substitute func(ctx context.Context, pinned AlbumRelease) (*AlbumRelease, error)
	// Library is the engine's library check (Engine.TrackBlocked). Required: without it no
	// album track other than the single can be placed safely (Astra 9).
	Library AlbumLibrary
}

// AlbumLibrary says whether an album track may be placed. It is satisfied by *Engine, which
// answers through the same library search the request flow uses and the active-item claim
// on the recording. A non-empty reason means the track must not be placed.
type AlbumLibrary interface {
	TrackBlocked(ctx context.Context, rec Recording) (reason string, err error)
}

// Album places full albums.
type Album struct{ cfg AlbumConfig }

// NewAlbum builds an Album over a bound Placement.
func NewAlbum(cfg AlbumConfig) (*Album, error) {
	if cfg.Placement == nil {
		return nil, errors.New("musicengine album: a Placement is required")
	}
	return &Album{cfg: cfg}, nil
}

// AlbumPlan is what "get the full album" would do for one placed single.
type AlbumPlan struct {
	ItemID     int64
	Pinned     AlbumRelease
	Dir        string // the release's folder in staging; "" when it is gone
	InStaging  bool
	Substitute *AlbumRelease // what would be fetched; set when the release is not in staging
}

// AlbumSkip is a track that was not placed, and why.
type AlbumSkip struct{ Path, Reason string }

// AlbumResult says what one Place did.
type AlbumResult struct {
	Plan AlbumPlan
	// NeedsConfirmation: nothing was placed; a decision shows Blair the substitute release.
	NeedsConfirmation bool
	DecisionRaised    bool
	Placed            []string // library paths published and confirmed by Plex
	Pending           []string // published, Plex has not listed them yet
	Skipped           []AlbumSkip
	// Discarded is the album's copy of the single's track, not used because the placed
	// single is at least as good.
	Discarded []string
	// Better is the album's copy of the single's track when it is strictly better than the
	// placed single: it is not placed here (that is a swap, U11) but is reported.
	Better   []string
	Repaired bool // the tags' disc/track numbers were not usable and the set was renumbered
}

type albumFile struct {
	Path string
	Tags TrackTags
	Res  audiocheck.Result
}

func (a *Album) db() *sql.DB { return a.cfg.Placement.m.DB() }

// Plan pins the release the single came from and says whether it is still in staging.
func (a *Album) Plan(ctx context.Context, itemID int64) (AlbumPlan, error) {
	p := a.cfg.Placement
	if p.m == nil {
		return AlbumPlan{}, errors.New("musicengine album: placement is not bound")
	}
	it, err := getItem(ctx, a.db(), itemID)
	if err != nil {
		return AlbumPlan{}, err
	}
	if it.State != StateDone {
		return AlbumPlan{}, fmt.Errorf("musicengine album: item %d is %s; the single must be placed first", it.ID, it.State)
	}
	cand, ok, err := placeChosen(ctx, a.db(), it.ID)
	if err != nil {
		return AlbumPlan{}, err
	}
	if !ok {
		return AlbumPlan{}, fmt.Errorf("musicengine album: item %d has no chosen candidate", it.ID)
	}
	plan := AlbumPlan{ItemID: it.ID}
	dir := filepath.Dir(cand.StagingPath)
	if a.cfg.Staging != nil {
		dir = a.cfg.Staging.CandidateDir(it.ID, cand.ID)
	}
	if n := albumAudioCount(dir); n > 0 {
		plan.Dir, plan.InStaging = dir, true
	}
	if cand.ReleaseMBID == "" || a.cfg.Releases == nil {
		return AlbumPlan{}, ErrAlbumUnavailable
	}
	rel, err := a.cfg.Releases.GetRelease(ctx, cand.ReleaseMBID)
	if err != nil {
		return AlbumPlan{}, fmt.Errorf("looking up release %s: %w", cand.ReleaseMBID, err)
	}
	plan.Pinned = AlbumRelease{
		ReleaseMBID: rel.MBID, Artist: rel.Artist, Title: rel.Title, TrackCount: len(rel.Tracks),
		Edition: strings.TrimSpace(strings.Join(nonEmpty(rel.Status, rel.Country, rel.Date), " ")),
	}
	if !plan.InStaging {
		sub := plan.Pinned
		if a.cfg.Substitute != nil {
			s, err := a.cfg.Substitute(ctx, plan.Pinned)
			if err != nil {
				return AlbumPlan{}, fmt.Errorf("finding a substitute release: %w", err)
			}
			if s != nil {
				sub = *s
			}
		}
		plan.Substitute = &sub
	}
	return plan, nil
}

func nonEmpty(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func albumAudioCount(dir string) int {
	n := 0
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && stagingAudioExts[strings.ToLower(filepath.Ext(p))] {
			n++
		}
		return nil
	})
	return n
}

// AlbumOptions carries Blair's confirmation.
type AlbumOptions struct {
	// Confirmed says Blair has seen the substitute release; StagingDir is where it was fetched.
	Confirmed  bool
	StagingDir string
}

// Place gets the full album. Without a release in staging and without confirmation it places
// nothing and raises the decision.
func (a *Album) Place(ctx context.Context, itemID int64, opts AlbumOptions) (AlbumResult, error) {
	plan, err := a.Plan(ctx, itemID)
	res := AlbumResult{Plan: plan}
	if err != nil {
		return res, err
	}
	dir := plan.Dir
	if !plan.InStaging {
		if !opts.Confirmed {
			res.NeedsConfirmation = true
			if err := a.cfg.Placement.cfg.Decisions.Raise(ctx, a.db(), DecisionRequest{
				ItemID: itemID, Kind: DecisionAlbumSubstitute, Now: a.cfg.Placement.m.Now(),
				Reason: fmt.Sprintf("The download of the album this single came from is gone. Getting the full album would download this instead: %s.",
					plan.Substitute),
				Options: []string{OptionApprove, OptionDrop},
			}); err != nil {
				return res, err
			}
			res.DecisionRaised = true
			return res, nil
		}
		if opts.StagingDir == "" || albumAudioCount(opts.StagingDir) == 0 {
			return res, errors.New("musicengine album: confirmed, but the substitute release has not been fetched into a staging dir yet")
		}
		dir = opts.StagingDir
	}
	return a.placeFrom(ctx, itemID, dir, res)
}

// ConfirmAlbum is Blair's yes to an album_substitute decision: stagingDir is where the
// fetched substitute release now sits. The decision is closed only once placement ran.
func (a *Album) ConfirmAlbum(ctx context.Context, decisionID int64, stagingDir string) (AlbumResult, error) {
	var itemID, answered int64
	var kind string
	err := a.db().QueryRowContext(ctx, `SELECT item_id, kind, answered_at FROM mus_decisions WHERE id = ?`, decisionID).Scan(&itemID, &kind, &answered)
	if errors.Is(err, sql.ErrNoRows) {
		return AlbumResult{}, fmt.Errorf("decision %d not found", decisionID)
	}
	if err != nil {
		return AlbumResult{}, fmt.Errorf("reading decision %d: %w", decisionID, err)
	}
	if kind != DecisionAlbumSubstitute || answered != 0 {
		return AlbumResult{}, fmt.Errorf("decision %d is not an open album confirmation", decisionID)
	}
	res, err := a.Place(ctx, itemID, AlbumOptions{Confirmed: true, StagingDir: stagingDir})
	if err != nil {
		return res, err
	}
	_, err = a.db().ExecContext(ctx, `UPDATE mus_decisions SET answer = ?, answered_at = ? WHERE id = ? AND answered_at = 0`,
		OptionApprove, a.cfg.Placement.m.Now().Unix(), decisionID)
	if err != nil {
		return res, fmt.Errorf("closing decision %d: %w", decisionID, err)
	}
	return res, nil
}

// albumFiles reads tags and checks every audio file in dir. A file that cannot be read is
// kept with a Blind result so it is skipped with a reason, not dropped silently.
func (a *Album) albumFiles(ctx context.Context, dir string) ([]albumFile, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && stagingAudioExts[strings.ToLower(filepath.Ext(p))] {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", dir, err)
	}
	sort.Strings(paths)
	p := a.cfg.Placement
	out := make([]albumFile, 0, len(paths))
	for _, path := range paths {
		f := albumFile{Path: path}
		tags, terr := p.readTags(ctx, path)
		if terr != nil {
			f.Res = audiocheck.Result{Path: path, Verdict: audiocheck.Blind, Reason: "Couldn't read the file's tags: " + terr.Error()}
			out = append(out, f)
			continue
		}
		f.Tags = tags
		f.Res = p.cfg.Checker.Check(ctx, path)
		out = append(out, f)
	}
	return out, nil
}

func (a *Album) placeFrom(ctx context.Context, itemID int64, dir string, res AlbumResult) (AlbumResult, error) {
	p := a.cfg.Placement
	it, err := getItem(ctx, a.db(), itemID)
	if err != nil {
		return res, err
	}
	rec := Recording{}
	if it.RecordingID != 0 {
		if rec, err = getRecording(ctx, a.db(), it.RecordingID); err != nil {
			return res, err
		}
	}
	files, err := a.albumFiles(ctx, dir)
	if err != nil {
		return res, err
	}
	if len(files) == 0 {
		return res, fmt.Errorf("musicengine album: no audio files in %s", dir)
	}
	if err := p.layout.Check(); err != nil {
		return res, err
	}

	// Numbering over every file, usable or not, so skipped tracks do not shift the rest.
	lt := make([]layoutTrack, len(files))
	for i, f := range files {
		lt[i] = layoutTrack{Key: f.Path, Disc: f.Tags.Disc, Track: f.Tags.Track}
	}
	nums, repaired := layoutNumberAlbum(lt)
	res.Repaired = repaired
	numOf := map[string]layoutNumber{}
	for _, n := range nums {
		numOf[n.Key] = n
	}
	albumArtist, album := albumIdentity(files, res.Plan.Pinned)

	placed, err := a.placedTrack(ctx, it)
	if err != nil {
		return res, err
	}
	if a.cfg.Library == nil {
		return res, errors.New("musicengine album: no library check is wired, so no album track can be placed without risking a duplicate")
	}
	var errs []error
	for _, f := range files {
		switch {
		case albumIsSingle(f, rec):
			if albumStrictlyBetter(f.Res, placed) {
				res.Better = append(res.Better, f.Path)
			} else {
				res.Discarded = append(res.Discarded, f.Path)
			}
			continue
		case f.Res.Verdict != audiocheck.Genuine:
			res.Skipped = append(res.Skipped, AlbumSkip{Path: f.Path, Reason: fmt.Sprintf("%s: %s", f.Res.Verdict, clip(f.Res.Reason, 200))})
			continue
		}
		num := numOf[f.Path]
		title := firstNonEmpty(f.Tags.Title, strings.TrimSuffix(filepath.Base(f.Path), filepath.Ext(f.Path)))
		plan, err := p.layout.Plan(albumArtist, album, num.Continuous, title, filepath.Ext(f.Path))
		if err != nil {
			res.Skipped = append(res.Skipped, AlbumSkip{Path: f.Path, Reason: err.Error()})
			continue
		}
		if sha, err := placeSHA256(plan.Path); err == nil {
			if src, serr := placeSHA256(f.Path); serr == nil && src == sha {
				res.Placed = append(res.Placed, plan.Path) // an earlier run already placed it
				continue
			}
			res.Skipped = append(res.Skipped, AlbumSkip{Path: f.Path, Reason: plan.Path + " already holds a different file, so this one was left alone."})
			continue
		}
		// A track whose recording the library already holds (or another item is getting) is
		// not placed again; a lossy or flagged copy there would need a swap, which the album
		// flow defers, so it is recorded rather than placed beside.
		blocked, err := a.trackBlocked(ctx, f)
		if err != nil {
			errs = append(errs, fmt.Errorf("checking the library for %s: %w", f.Path, err))
			continue
		}
		if blocked != "" {
			res.Skipped = append(res.Skipped, AlbumSkip{Path: f.Path, Reason: blocked})
			continue
		}
		out, err := a.placeOne(ctx, f, plan.Path, num, rec)
		switch {
		case err != nil:
			var ve *VerifyError
			if errors.As(err, &ve) {
				res.Skipped = append(res.Skipped, AlbumSkip{Path: f.Path, Reason: ve.Reason})
				continue
			}
			errs = append(errs, fmt.Errorf("placing %s: %w", f.Path, err))
		case out.pending:
			res.Pending = append(res.Pending, plan.Path)
		case out.stopped:
			res.Skipped = append(res.Skipped, AlbumSkip{Path: f.Path, Reason: "Stopped: " + out.reason})
		default:
			res.Placed = append(res.Placed, plan.Path)
		}
	}
	return res, errors.Join(errs...)
}

// trackBlocked identifies the album track's recording (the tag's MusicBrainz id, matched to a
// known recording row when there is one) and asks the library check whether it may be placed.
func (a *Album) trackBlocked(ctx context.Context, f albumFile) (string, error) {
	rec := Recording{
		MBID: f.Tags.RecordingMBID, Title: f.Tags.Title, Artist: firstNonEmpty(f.Tags.Artist, f.Tags.AlbumArtist),
		LengthMs:    int(f.Res.Facts.DurationSec * 1000),
		VersionHint: versionHintString(versionHints(f.Tags.Title)),
	}
	if rec.MBID != "" {
		known, err := scanRecording(a.db().QueryRowContext(ctx, `SELECT `+recCols+` FROM mus_recordings WHERE mbid = ?`, rec.MBID))
		switch {
		case err == nil:
			rec = known
		case !errors.Is(err, sql.ErrNoRows):
			return "", fmt.Errorf("looking up recording %s: %w", rec.MBID, err)
		}
	}
	return a.cfg.Library.TrackBlocked(ctx, rec)
}

type albumPlaced struct {
	pending, stopped bool
	reason           string
}

// placeOne takes one album track through Stage, Advance and the Plex confirmation.
func (a *Album) placeOne(ctx context.Context, f albumFile, dest string, num layoutNumber, single Recording) (albumPlaced, error) {
	p := a.cfg.Placement
	recID := int64(0)
	if mbid := f.Tags.RecordingMBID; mbid != "" {
		err := a.db().QueryRowContext(ctx, `SELECT id FROM mus_recordings WHERE mbid = ?`, mbid).Scan(&recID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return albumPlaced{}, fmt.Errorf("looking up recording %s: %w", mbid, err)
		}
	}
	row, err := p.Stage(ctx, StageSpec{
		Kind: JournalPlace, RecordingID: recID, Source: f.Path, Dest: dest,
		ExpectFingerprint: f.Res.Fingerprint, Number: &num,
	})
	if errors.Is(err, ErrSpaceWait) {
		// No journal row was started, so nothing resumes this track; asking for the album
		// again places it once there is room (earlier tracks are reported as placed).
		return albumPlaced{stopped: true, reason: "Not enough free disk space. Ask for the album again later."}, nil
	}
	if err != nil {
		return albumPlaced{}, err
	}
	out, err := p.Advance(ctx, row)
	if errors.Is(err, ErrSpaceWait) {
		return albumPlaced{pending: true}, nil // the open row is advanced by Swaps.Resume
	}
	if err != nil {
		return albumPlaced{}, err
	}
	switch out {
	case OutcomePending:
		return albumPlaced{pending: true}, nil
	case OutcomeRolledBack:
		return albumPlaced{stopped: true, reason: "The track was rolled back; nothing was added to your library."}, nil
	case OutcomeStopped:
		return albumPlaced{stopped: true, reason: "Another file already has that name."}, nil
	}
	row, err = p.jr.Get(ctx, row.ID)
	if err != nil {
		return albumPlaced{}, err
	}
	tags, err := p.readTags(ctx, row.NewPath)
	if err != nil {
		return albumPlaced{}, err
	}
	key, indexed, err := p.plexIndexed(ctx, row, tags, true)
	if err != nil {
		return albumPlaced{}, err
	}
	if !indexed {
		return albumPlaced{pending: true}, nil
	}
	if err := p.confirm(ctx, row, tags, key, albumFacts(f.Res, row.PublishedSHA), false); err != nil {
		return albumPlaced{}, err
	}
	return albumPlaced{}, nil
}

func albumFacts(r audiocheck.Result, sha string) *placeFacts {
	f := &placeFacts{
		Codec: r.Facts.Codec, Container: r.Facts.Container, BitDepth: r.Facts.BitDepth, SampleRate: r.Facts.SampleRate,
		Channels: r.Facts.Channels, Bitrate: int(r.Facts.BitrateKbps), DurationMs: int64(r.Facts.DurationSec * 1000),
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

// albumIdentity picks the folder artist and album: the album-artist tag the tracks agree on,
// "Various Artists" when the track artists differ and no album artist says otherwise (a
// compilation is filed under the compilation), else the pinned release's own names.
func albumIdentity(files []albumFile, pinned AlbumRelease) (artist, album string) {
	countA, countAlb := map[string]int{}, map[string]int{}
	artists := map[string]bool{}
	for _, f := range files {
		if f.Tags.AlbumArtist != "" {
			countA[f.Tags.AlbumArtist]++
		}
		if f.Tags.Artist != "" {
			artists[f.Tags.Artist] = true
		}
		if f.Tags.Album != "" {
			countAlb[f.Tags.Album]++
		}
	}
	artist = albumMajority(countA)
	if artist == "" {
		switch {
		case len(artists) > 1:
			artist = "Various Artists"
		case len(artists) == 1:
			for a := range artists {
				artist = a
			}
		default:
			artist = pinned.Artist
		}
	}
	album = firstNonEmpty(albumMajority(countAlb), pinned.Title)
	return artist, album
}

func albumMajority(m map[string]int) string {
	best, n := "", 0
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if m[k] > n {
			best, n = k, m[k]
		}
	}
	return best
}

// albumIsSingle reports whether a file is the recording the single was placed for: the
// MusicBrainz recording id in its tags, or the same title and a length within 3 seconds.
func albumIsSingle(f albumFile, rec Recording) bool {
	if rec.MBID == "" {
		return false
	}
	if f.Tags.RecordingMBID != "" {
		return strings.EqualFold(f.Tags.RecordingMBID, rec.MBID)
	}
	if acqNorm(f.Tags.Title) != acqNorm(rec.Title) || rec.Title == "" {
		return false
	}
	if rec.LengthMs == 0 || f.Res.Facts.DurationSec == 0 {
		return true
	}
	d := float64(rec.LengthMs)/1000 - f.Res.Facts.DurationSec
	return d > -3 && d < 3
}

// placedFacts are the quality dimensions of the library copy of the single.
type placedFacts struct {
	Codec                string
	BitDepth, SampleRate int
	Found                bool
}

func (a *Album) placedTrack(ctx context.Context, it Item) (placedFacts, error) {
	if it.TrackID == 0 {
		return placedFacts{}, nil
	}
	var f placedFacts
	err := a.db().QueryRowContext(ctx, `SELECT codec, bit_depth, sample_rate FROM mus_library_tracks WHERE id = ?`, it.TrackID).
		Scan(&f.Codec, &f.BitDepth, &f.SampleRate)
	if errors.Is(err, sql.ErrNoRows) {
		return placedFacts{}, nil
	}
	if err != nil {
		return placedFacts{}, fmt.Errorf("reading the placed single: %w", err)
	}
	f.Found = true
	return f, nil
}

// albumStrictlyBetter applies the no-worse, at-least-one-better rule (R21) to the album's
// copy of the single's track against the placed single. A copy that is not genuine, or that
// cannot be compared, is not better.
func albumStrictlyBetter(n audiocheck.Result, placed placedFacts) bool {
	if !placed.Found || n.Verdict != audiocheck.Genuine {
		return false
	}
	nl, pl := isLossless(n.Facts.Codec), isLossless(placed.Codec)
	if pl && !nl {
		return false
	}
	if n.Facts.BitDepth < placed.BitDepth || n.Facts.SampleRate < placed.SampleRate {
		return false
	}
	return (nl && !pl) || n.Facts.BitDepth > placed.BitDepth || n.Facts.SampleRate > placed.SampleRate
}
