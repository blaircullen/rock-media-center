package musicengine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tristenlammi/arrmada/internal/audiocheck"
	"github.com/tristenlammi/arrmada/internal/metadata"
	"github.com/tristenlammi/arrmada/internal/plex"
	"github.com/tristenlammi/arrmada/internal/store"
	"github.com/tristenlammi/arrmada/internal/toolrun"
)

// Placement is the engine's Placer (U10). It publishes a verified file into the library
// through the journal (KTD7, KTD18) and confirms Plex has indexed it (R17). It also owns the
// phases a swap or undo shares with a placement, which U11 drives through Stage and Advance.
//
// Placement flow, each phase recorded only after its result is verified:
//
//	staged          the chosen candidate's staged file still has the checksum it was checked with
//	temp_verified   copied to .rmc-tmp-* in the target folder, tagged, owned, checksummed and
//	                fully decoded in place
//	(quarantine_verified   swap/undo only: the incumbent's copy in quarantine matches)
//	published       a no-replace hard link (new track) or a checked rename (swap/undo)
//	plex_confirmed  Plex lists the track under the right artist and album

// Plex is the slice of internal/plex placement reads and writes.
type PlacePlex interface {
	RefreshPath(ctx context.Context, sectionKey, dir string) error
	SearchTracks(ctx context.Context, sectionKey, artist, title string) ([]plex.Track, error)
}

// PlexWriter is the slice of internal/plex the mirror and the remap use.
type PlexWriter interface {
	Tracks(ctx context.Context, sectionKey string) ([]plex.Track, error)
	SearchTracks(ctx context.Context, sectionKey, artist, title string) ([]plex.Track, error)
	Playlists(ctx context.Context) ([]plex.Playlist, error)
	PlaylistItems(ctx context.Context, playlistKey string) ([]plex.PlaylistItem, error)
	CreatePlaylist(ctx context.Context, title string, ratingKeys []string) (string, error)
	AddToPlaylist(ctx context.Context, playlistKey string, ratingKeys []string) error
	RemoveFromPlaylist(ctx context.Context, playlistKey, playlistItemID string) error
	SetRating(ctx context.Context, ratingKey string, rating float64) error
}

// ReleaseLookup is the MusicBrainz release read used to find a track's disc position when
// the file's own tags lack it. metadata.MusicRecordings satisfies it.
type ReleaseLookup interface {
	GetRelease(ctx context.Context, mbid string) (*metadata.ReleaseDetails, error)
}

var (
	_ PlacePlex     = (*plex.Client)(nil)
	_ PlexWriter    = (*plex.Client)(nil)
	_ ReleaseLookup = metadata.MusicRecordings(nil)
	_ Placer        = (*Placement)(nil)
)

// TrackTags are the tags placement needs from a file.
type TrackTags struct {
	Artist, AlbumArtist, Album, Title string
	Disc, Track                       int
	RecordingMBID                     string
}

// TagEditor reads and fixes FLAC tags (metaflac through internal/toolrun in production).
type TagEditor interface {
	Read(ctx context.Context, path string) (TrackTags, error)
	// WriteNumbering replaces the file's disc and track tags.
	WriteNumbering(ctx context.Context, path string, disc, track int) error
}

// Metaflac is the production TagEditor.
type Metaflac struct {
	// Timeout bounds one call; 0 means 60 s.
	Timeout time.Duration
}

var _ TagEditor = Metaflac{}

func (m Metaflac) run(ctx context.Context, args ...string) ([]byte, error) {
	to := m.Timeout
	if to <= 0 {
		to = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	cmd := toolrun.Command(ctx, toolrun.Normal, "metaflac", args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("metaflac %s: %w: %s", strings.Join(args[:min(len(args), 1)], " "), err, clip(errb.String(), 300))
	}
	return out.Bytes(), nil
}

// Read lists the Vorbis comments with metaflac.
func (m Metaflac) Read(ctx context.Context, path string) (TrackTags, error) {
	out, err := m.run(ctx, "--export-tags-to=-", path)
	if err != nil {
		return TrackTags{}, err
	}
	return parseVorbisTags(out), nil
}

// WriteNumbering removes every spelling of the disc and track tags, then sets the standard
// Vorbis names, in one metaflac call (the getmusic skill's fix).
func (m Metaflac) WriteNumbering(ctx context.Context, path string, disc, track int) error {
	_, err := m.run(ctx,
		"--remove-tag=TRACKNUMBER", "--remove-tag=DISCNUMBER", "--remove-tag=track", "--remove-tag=disc",
		"--set-tag=TRACKNUMBER="+strconv.Itoa(track), "--set-tag=DISCNUMBER="+strconv.Itoa(disc), path)
	return err
}

func parseVorbisTags(b []byte) TrackTags {
	var t TrackTags
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.ToUpper(strings.TrimSpace(k)) {
		case "ARTIST":
			if t.Artist == "" {
				t.Artist = v
			}
		case "ALBUMARTIST", "ALBUM ARTIST":
			t.AlbumArtist = v
		case "ALBUM":
			t.Album = v
		case "TITLE":
			t.Title = v
		case "TRACKNUMBER", "TRACK":
			t.Track = placeLeadingInt(v)
		case "DISCNUMBER", "DISC":
			t.Disc = placeLeadingInt(v)
		case "MUSICBRAINZ_TRACKID":
			t.RecordingMBID = strings.ToLower(v)
		}
	}
	return t
}

// placeLeadingInt reads "3" and "3/12" as 3.
func placeLeadingInt(s string) int {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "/")
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

// PlacementConfig wires a Placement.
type PlacementConfig struct {
	// LibraryRoot is the library as this process sees it; PlexRoot is the same folder as the
	// Plex server sees it (empty means the same).
	LibraryRoot, PlexRoot string
	// QuarantineRoot is the quarantine volume's root (KTD9); it must already exist.
	QuarantineRoot string
	// PlexSection is the Plex music section key, read each time.
	PlexSection func() string

	Plex     PlacePlex
	Checker  AudioChecker
	Tags     TagReader // fallback for non-FLAC files
	Editor   TagEditor
	Releases ReleaseLookup // optional: disc position when the tags lack it
	Staging  *Staging      // optional: widens the sibling search to the whole candidate dir
	Mirror   *Mirror       // optional: synced after each placement
	// Decisions raises collision decisions; nil means the table writer.
	Decisions DecisionRaiser
	// Swaps handles the swapping and waiting_for_playback states (U11). Nil means those
	// states are "handled by U11": Step does nothing for them.
	Swaps Placer
	// Finalize runs for swap and undo rows after Plex lists the file and before plex_confirmed
	// is recorded: U11 re-applies the track's rating and memberships and reads them back.
	Finalize func(ctx context.Context, row JournalRow, ratingKey string) error

	// Owner and modes for files and folders (R17); defaults 1024:100, 0664, 0775.
	UID, GID          int
	FileMode, DirMode os.FileMode
	Chown             func(path string, uid, gid int) error // default os.Chown
	Euid              func() int                            // default os.Geteuid
	Measure           func(path string) bool                // default diskspace.Of
	PollAttempts      int                                   // Plex index polls per Step, default 5
	PollInterval      time.Duration                         // default 3 s
	RetryAfter        time.Duration                         // wait before the next Step while Plex lags, default 30 s
	Sleep             func(ctx context.Context, d time.Duration)
	Log               *slog.Logger
}

// Placement implements Placer for placing and awaiting_plex_index. See the file comment.
type Placement struct {
	cfg    PlacementConfig
	layout *Layout
	paths  pathMap
	m      Machine
	jr     *Journal
	log    *slog.Logger

	// crash, when set (tests), is asked at each named point; true makes the code return
	// errCrash at once, leaving disk and journal exactly as they are.
	crash func(point string) bool
}

var errCrash = errors.New("musicengine: simulated crash")

// NewPlacement validates the config and builds a Placement. Call Bind (or let the first
// Step do it) before Reconcile.
func NewPlacement(cfg PlacementConfig) (*Placement, error) {
	if cfg.LibraryRoot == "" || !filepath.IsAbs(cfg.LibraryRoot) {
		return nil, fmt.Errorf("musicengine placement: library root %q must be an absolute path", cfg.LibraryRoot)
	}
	if cfg.Checker == nil || cfg.Plex == nil {
		return nil, errors.New("musicengine placement: a checker and a Plex client are required")
	}
	if cfg.PlexSection == nil {
		return nil, errors.New("musicengine placement: no Plex section setting")
	}
	if cfg.UID == 0 && cfg.GID == 0 {
		cfg.UID, cfg.GID = 1024, 100
	}
	if cfg.FileMode == 0 {
		cfg.FileMode = 0o664
	}
	if cfg.DirMode == 0 {
		cfg.DirMode = 0o775
	}
	if cfg.Chown == nil {
		cfg.Chown = os.Chown
	}
	if cfg.Euid == nil {
		cfg.Euid = os.Geteuid
	}
	if cfg.PollAttempts <= 0 {
		cfg.PollAttempts = 5
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 3 * time.Second
	}
	if cfg.RetryAfter <= 0 {
		cfg.RetryAfter = 30 * time.Second
	}
	if cfg.Sleep == nil {
		cfg.Sleep = func(ctx context.Context, d time.Duration) {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
			case <-t.C:
			}
		}
	}
	if cfg.Decisions == nil {
		cfg.Decisions = tableDecisions{}
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	root := filepath.Clean(cfg.LibraryRoot)
	return &Placement{
		cfg:    cfg,
		layout: &Layout{Root: root, Measure: cfg.Measure},
		paths:  pathMap{local: root, plex: cleanOr(cfg.PlexRoot, root)},
		log:    log,
	}, nil
}

func cleanOr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return filepath.Clean(s)
}

// Bind gives the Placement its machine (the Engine). Step binds on its own.
func (p *Placement) Bind(m Machine) {
	if p.m == m && p.jr != nil {
		return
	}
	p.m = m
	p.jr = NewJournal(m.DB(), m.Now)
}

// Journal is the journal U11 uses for swaps and undo; nil until bound.
func (p *Placement) Journal() *Journal { return p.jr }

// Layout is the library layout (root fence, artist lookup).
func (p *Placement) Layout() *Layout { return p.layout }

func (p *Placement) at(point string) error {
	if p.crash != nil && p.crash(point) {
		return fmt.Errorf("%w at %s", errCrash, point)
	}
	return nil
}

// ---- Step ----

// Step is the Placer. Same idempotence rule as the Acquirer: after a restart the item comes
// back in the same state, and the open journal row says where it was.
func (p *Placement) Step(ctx context.Context, m Machine, it Item) error {
	p.Bind(m)
	switch it.State {
	case StatePlacing:
		return p.stepPlacing(ctx, it)
	case StateAwaitingPlexIndex:
		return p.stepAwaiting(ctx, it)
	case StateSwapping, StateWaitingForPlayback:
		if p.cfg.Swaps != nil {
			return p.cfg.Swaps.Step(ctx, m, it)
		}
		p.log.Debug("musicengine placement: swapping and waiting_for_playback are handled by U11; nothing to do", "item", it.ID, "state", it.State)
		return nil
	}
	return nil
}

// ---- hashing, copying, permissions ----

func placeSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// placeCopy copies src over dst (truncating a partial earlier copy), syncs it, and calls
// midPoint once half the bytes are written so crash tests can stop mid-copy.
func (p *Placement) placeCopy(src, dst, midPoint string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	fi, err := in.Stat()
	if err != nil {
		out.Close()
		return err
	}
	half := fi.Size() / 2
	if _, err := io.CopyN(out, in, half); err != nil && !errors.Is(err, io.EOF) {
		out.Close()
		return err
	}
	if err := p.at(midPoint); err != nil {
		out.Close() // a real crash would leave the half-written file; so do we
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// placePerms sets the mode and, when running as root, the owner (R17). A non-root process
// cannot chown to another user; that is recorded, not fatal.
func (p *Placement) placePerms(path string, dir bool) (skippedChown string, err error) {
	mode := p.cfg.FileMode
	if dir {
		mode = p.cfg.DirMode
	}
	if err := os.Chmod(path, mode); err != nil {
		return "", fmt.Errorf("setting mode on %s: %w", path, err)
	}
	if p.cfg.Euid() != 0 {
		return fmt.Sprintf("chown %d:%d skipped: not running as root (euid %d)", p.cfg.UID, p.cfg.GID, p.cfg.Euid()), nil
	}
	if err := p.cfg.Chown(path, p.cfg.UID, p.cfg.GID); err != nil {
		return "", fmt.Errorf("setting owner on %s: %w", path, err)
	}
	return "", nil
}

// ---- verification ----

// VerifyError means the file was looked at and is bad (damaged, or its audio changed). The
// row is rolled back and the candidate must not be tried again. A Blind check is not this
// error: it could not look, so it is retried.
type VerifyError struct{ Reason string }

func (e *VerifyError) Error() string {
	return "musicengine placement: verification failed: " + e.Reason
}

// verifyInPlace checksums and fully decodes the temp file where it sits (KTD7), so the
// verified checksum is the file that gets published.
func (p *Placement) verifyInPlace(ctx context.Context, temp, expectFP string) (string, audiocheck.Result, error) {
	sha, err := placeSHA256(temp)
	if err != nil {
		return "", audiocheck.Result{}, fmt.Errorf("checksumming %s: %w", temp, err)
	}
	res := p.cfg.Checker.Check(ctx, temp)
	switch {
	case res.Verdict == audiocheck.Blind:
		return "", res, fmt.Errorf("check of %s was blind (will retry): %s", filepath.Base(temp), res.Reason)
	case res.Verdict == audiocheck.Corrupt || !res.Integrity.OK:
		return "", res, &VerifyError{Reason: "integrity failed: " + clip(res.Reason+" "+res.Integrity.Detail, 300)}
	case expectFP != "" && res.Fingerprint != expectFP:
		return "", res, &VerifyError{Reason: "the decoded audio differs from the checked candidate's (fingerprint mismatch)"}
	}
	return sha, res, nil
}

// ---- Stage: phases 0 to 2 ----

// StageSpec is one file to bring to temp_verified. U11 uses it for swaps (Source is the
// checked candidate) and undo (Source is the quarantined version, TagFix false).
type StageSpec struct {
	Kind                         string
	ItemID, TrackID, CandidateID int64
	RecordingID                  int64
	Source                       string
	SourceSHA                    string // expected checksum of Source; "" skips the cross-check
	Dest                         string // library path
	IncumbentSHA                 string // swap/undo: what Dest must hold now
	QuarantinePath               string // swap/undo
	ExpectFingerprint            string
	OldFingerprint               string
	RatingBefore                 *float64
	MembershipsBefore            string
	PreviousSwapID               int64
	Number                       *layoutNumber // tag numbering to write; nil leaves tags alone
}

// Stage runs phases 0 to 2: open the row, check the staged source, copy it to a temp name in
// the target folder, fix tags, set mode and owner, then checksum and fully decode it in place.
// A bad result rolls the row back and returns *VerifyError. A failure to look returns an
// error and leaves the row for the next pass to settle.
func (p *Placement) Stage(ctx context.Context, s StageSpec) (JournalRow, error) {
	if p.jr == nil {
		return JournalRow{}, errors.New("musicengine placement: not bound to a machine")
	}
	if err := p.layout.Check(); err != nil {
		return JournalRow{}, err
	}
	if err := p.layout.Inside(s.Dest); err != nil {
		return JournalRow{}, err
	}
	swap := s.Kind != JournalPlace
	if swap {
		if s.IncumbentSHA == "" || s.QuarantinePath == "" {
			return JournalRow{}, errors.New("musicengine placement: a swap needs the incumbent checksum and a quarantine path")
		}
		if _, err := os.Stat(s.Dest); err != nil {
			return JournalRow{}, fmt.Errorf("swap destination %s: %w", s.Dest, err)
		}
	}
	b := JournalBegin{
		Kind: s.Kind, ItemID: s.ItemID, TrackID: s.TrackID, CandidateID: s.CandidateID, RecordingID: s.RecordingID,
		NewPath: s.Dest, IncumbentSHA: s.IncumbentSHA, QuarantinePath: s.QuarantinePath,
		NewFingerprint: s.ExpectFingerprint, OldFingerprint: s.OldFingerprint,
		RatingBefore: s.RatingBefore, MembershipsBefore: s.MembershipsBefore, PreviousSwapID: s.PreviousSwapID,
	}
	if swap {
		b.OldPath = s.Dest
	}
	row, err := p.jr.Start(ctx, b)
	if err != nil {
		return JournalRow{}, err
	}
	if err := p.at("stage:begin"); err != nil {
		return row, err
	}
	var notes []string
	if err := p.layout.MkdirInside(filepath.Dir(s.Dest), p.cfg.DirMode, func(path string) error {
		note, err := p.placePerms(path, true)
		if note != "" && len(notes) == 0 {
			notes = append(notes, note)
		}
		return err
	}); err != nil {
		return row, err
	}

	srcSHA, err := placeSHA256(s.Source)
	if err != nil {
		return row, p.stageBad(ctx, row, fmt.Sprintf("the staged file is unreadable: %v", err))
	}
	if s.SourceSHA != "" && srcSHA != s.SourceSHA {
		return row, p.stageBad(ctx, row, "the staged file no longer has the checksum it was checked with")
	}
	if _, err := p.jr.Record(ctx, p.m.DB(), row.ID, PhaseStaged, PhaseData{StagedSHA: srcSHA}); err != nil {
		return row, err
	}
	if err := p.at("stage:staged"); err != nil {
		return row, err
	}
	if err := p.placeCopy(s.Source, row.TempPath, "stage:mid_copy"); err != nil {
		return row, fmt.Errorf("copying %s to %s: %w", s.Source, row.TempPath, err)
	}
	if err := p.at("stage:copied"); err != nil {
		return row, err
	}

	if s.Number != nil && strings.EqualFold(filepath.Ext(s.Dest), ".flac") && p.cfg.Editor != nil {
		if err := p.cfg.Editor.WriteNumbering(ctx, row.TempPath, s.Number.Disc, s.Number.Continuous); err != nil {
			return row, fmt.Errorf("fixing disc and track tags on %s: %w", row.TempPath, err)
		}
	} else if s.Number != nil {
		notes = append(notes, "tag numbering left as downloaded: "+strings.ToLower(filepath.Ext(s.Dest))+" is not FLAC")
	}
	if err := p.at("stage:tagged"); err != nil {
		return row, err
	}
	// Mode and owner after metaflac, which can rewrite the file.
	if note, err := p.placePerms(row.TempPath, false); err != nil {
		return row, err
	} else if note != "" && len(notes) == 0 {
		notes = append(notes, note)
	}
	for _, n := range notes {
		if err := p.jr.Note(ctx, row.ID, n); err != nil {
			p.log.Warn("musicengine placement: could not note journal row", "row", row.ID, "err", err)
		}
	}

	sha, res, err := p.verifyInPlace(ctx, row.TempPath, s.ExpectFingerprint)
	if err != nil {
		var ve *VerifyError
		if errors.As(err, &ve) {
			return row, p.stageBad(ctx, row, ve.Reason)
		}
		return row, err
	}
	if _, err := p.jr.Record(ctx, p.m.DB(), row.ID, PhaseTempVerified, PhaseData{TempSHA: sha}); err != nil {
		return row, err
	}
	if res.Fingerprint != "" {
		if _, err := p.m.DB().ExecContext(ctx, `UPDATE mus_swap_journal SET new_pcm_fingerprint = ? WHERE id = ?`, res.Fingerprint, row.ID); err != nil {
			p.log.Warn("musicengine placement: could not store fingerprint", "row", row.ID, "err", err)
		}
	}
	if err := p.at("stage:temp_verified"); err != nil {
		return row, err
	}
	return p.jr.Get(ctx, row.ID)
}

// stageBad rolls the row back for a verified-bad result and returns the *VerifyError.
func (p *Placement) stageBad(ctx context.Context, row JournalRow, reason string) error {
	if _, err := p.rollback(ctx, row, reason); err != nil {
		return errors.Join(&VerifyError{Reason: reason}, err)
	}
	return &VerifyError{Reason: reason}
}

// ---- Outcome, rollback, stop ----

// Outcome is what Advance did with a row.
type Outcome int

const (
	// OutcomeAdvanced: the row reached published (or beyond) and waits for Plex.
	OutcomeAdvanced Outcome = iota
	// OutcomeRolledBack: nothing in the library changed; the row is closed.
	OutcomeRolledBack
	// OutcomeStopped: a collision or a changed incumbent; the row is closed, a decision raised.
	OutcomeStopped
)

func (o Outcome) String() string {
	return [...]string{"advanced", "rolled back", "stopped"}[o]
}

// rollback removes the row's temp file(s) and closes it. Library files are never touched.
func (p *Placement) rollback(ctx context.Context, row JournalRow, reason string) (Outcome, error) {
	if err := p.removeTemps(row); err != nil {
		return OutcomeRolledBack, err
	}
	if err := p.jr.Close(ctx, p.m.DB(), row.ID, "rolled back: "+reason); err != nil {
		return OutcomeRolledBack, err
	}
	return OutcomeRolledBack, nil
}

// removeTemps deletes the row's temp name and any other .rmc-tmp-<id>-* beside the destination.
func (p *Placement) removeTemps(row JournalRow) error {
	var errs []error
	if row.TempPath != "" {
		if err := os.Remove(row.TempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(row.NewPath))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.Join(errs...)
		}
		return errors.Join(append(errs, err)...)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), journalTempPrefix(row.ID)) {
			if err := os.Remove(filepath.Join(filepath.Dir(row.NewPath), e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// stop closes a row that must not go on (the destination holds something unexpected), raises
// a decision and parks the item. The destination is left exactly as found.
func (p *Placement) stop(ctx context.Context, row JournalRow, kind, reason string) (Outcome, error) {
	if err := p.removeTemps(row); err != nil {
		return OutcomeStopped, err
	}
	err := store.WithTx(ctx, p.m.DB(), func(tx *sql.Tx) error {
		if err := p.jr.Close(ctx, tx, row.ID, "stopped: "+reason); err != nil {
			return err
		}
		itemID := row.ItemID
		if itemID == 0 {
			var err error
			if itemID, err = placeLatestItemFor(ctx, tx, row.RecordingID); err != nil {
				return err
			}
		}
		if itemID == 0 {
			p.log.Warn("musicengine placement: stopped row has no item to attach a decision to", "row", row.ID, "reason", reason)
			return nil
		}
		if err := p.cfg.Decisions.Raise(ctx, tx, DecisionRequest{
			ItemID: itemID, Kind: kind, Reason: reason, Options: []string{OptionRetry, OptionDrop}, Now: p.m.Now(),
		}); err != nil {
			return err
		}
		return p.parkItem(ctx, tx, row.ItemID, reason)
	})
	return OutcomeStopped, err
}

// parkItem moves the row's own item to waiting_blair when its state allows.
func (p *Placement) parkItem(ctx context.Context, tx store.Execer, itemID int64, reason string) error {
	if itemID == 0 {
		return nil
	}
	it, err := getItem(ctx, tx, itemID)
	if err != nil {
		return err
	}
	if !allowed(it.State, StateWaitingBlair, State(it.PausedFromState)) {
		return nil
	}
	return p.m.TransitionTx(ctx, tx, itemID, StateWaitingBlair, clip(reason, 300))
}

// placeLatestItemFor is the newest item of a recording, or 0.
func placeLatestItemFor(ctx context.Context, q store.Execer, recordingID int64) (int64, error) {
	if recordingID == 0 {
		return 0, nil
	}
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM mus_items WHERE recording_id = ? ORDER BY id DESC LIMIT 1`, recordingID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("finding the item of recording %d: %w", recordingID, err)
	}
	return id, nil
}

// ---- Advance: phases 2 to 4 ----

// Advance takes a row forward from its last recorded phase, using what is on disk. It is the
// one code path for both Step and the reconciler, so a second pass over a settled row changes
// nothing. A row below temp_verified is rolled back (the table redoes it from the start).
func (p *Placement) Advance(ctx context.Context, row JournalRow) (Outcome, error) {
	if p.jr == nil {
		return OutcomeAdvanced, errors.New("musicengine placement: not bound to a machine")
	}
	if err := p.layout.Check(); err != nil {
		return OutcomeAdvanced, err // storage unreachable: touch nothing, leave the row
	}
	if err := p.layout.Inside(row.NewPath); err != nil {
		return OutcomeAdvanced, err
	}
	for {
		var err error
		switch {
		case !row.Open():
			return OutcomeAdvanced, nil
		case row.Phase < PhaseTempVerified:
			return p.rollback(ctx, row, "interrupted before the temp file was verified")
		case row.Phase == PhaseTempVerified && row.Kind == JournalPlace:
			return p.publishNew(ctx, row)
		case row.Phase == PhaseTempVerified:
			var out Outcome
			if out, row, err = p.quarantineIncumbent(ctx, row); err != nil || out != OutcomeAdvanced {
				return out, err
			}
		case row.Phase == PhaseQuarantineVerified:
			return p.publishReplace(ctx, row)
		case row.Phase == PhasePublished:
			if err := p.removeTemps(row); err != nil {
				return OutcomeAdvanced, err
			}
			if err := p.toAwaiting(ctx, p.m.DB(), row); err != nil {
				return OutcomeAdvanced, err
			}
			return OutcomeAdvanced, nil
		default:
			return OutcomeAdvanced, nil // plex_confirmed: nothing left but the close
		}
	}
}

// toAwaiting moves the row's item to awaiting_plex_index when it is still placing or swapping.
func (p *Placement) toAwaiting(ctx context.Context, q store.Execer, row JournalRow) error {
	if row.ItemID == 0 {
		return nil
	}
	it, err := getItem(ctx, q, row.ItemID)
	if err != nil {
		return err
	}
	if it.State != StatePlacing && it.State != StateSwapping {
		return nil
	}
	return p.m.TransitionTx(ctx, q, row.ItemID, StateAwaitingPlexIndex, "published; waiting for Plex to index")
}

func (p *Placement) recordPublished(ctx context.Context, row JournalRow, sha string) error {
	return store.WithTx(ctx, p.m.DB(), func(tx *sql.Tx) error {
		if _, err := p.jr.Record(ctx, tx, row.ID, PhasePublished, PhaseData{PublishedSHA: sha}); err != nil {
			return err
		}
		return p.toAwaiting(ctx, tx, row)
	})
}

// publishNew publishes a new track with an exclusive no-replace hard link (KTD18). A name
// that is already taken by anything other than our own earlier link is a collision: the
// existing file is never touched.
func (p *Placement) publishNew(ctx context.Context, row JournalRow) (Outcome, error) {
	for attempt := 0; attempt < 2; attempt++ {
		destInfo, derr := os.Lstat(row.NewPath)
		switch {
		case derr == nil:
			if p.isOurs(row, destInfo) {
				return p.finishPublished(ctx, row)
			}
			return p.stop(ctx, row, "collision", fmt.Sprintf("%s already exists and is not the file being placed; left untouched", row.NewPath))
		case !errors.Is(derr, os.ErrNotExist):
			return OutcomeAdvanced, fmt.Errorf("looking at %s: %w", row.NewPath, derr)
		}
		sha, err := placeSHA256(row.TempPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return p.rollback(ctx, row, "the verified temp file is gone")
			}
			return OutcomeAdvanced, fmt.Errorf("checksumming %s: %w", row.TempPath, err)
		}
		if sha != row.TempSHA {
			return p.rollback(ctx, row, "the temp file no longer has its verified checksum")
		}
		if err := os.Link(row.TempPath, row.NewPath); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue // lost a race for the name; look again
			}
			return OutcomeAdvanced, fmt.Errorf("publishing %s with a no-replace link: %w", row.NewPath, err)
		}
		if err := p.at("place:linked"); err != nil {
			return OutcomeAdvanced, err
		}
		return p.finishPublished(ctx, row)
	}
	return OutcomeAdvanced, fmt.Errorf("publishing %s: the name keeps changing under us", row.NewPath)
}

// isOurs reports whether the file at the destination is the one this row published: the same
// inode as the temp name, or the same bytes as the verified temp file.
func (p *Placement) isOurs(row JournalRow, destInfo fs.FileInfo) bool {
	if ti, err := os.Lstat(row.TempPath); err == nil && os.SameFile(ti, destInfo) {
		return true
	}
	if !destInfo.Mode().IsRegular() {
		return false
	}
	sha, err := placeSHA256(row.NewPath)
	return err == nil && sha == row.TempSHA
}

func (p *Placement) finishPublished(ctx context.Context, row JournalRow) (Outcome, error) {
	sha, err := placeSHA256(row.NewPath)
	if err != nil {
		return OutcomeAdvanced, fmt.Errorf("checksumming published %s: %w", row.NewPath, err)
	}
	if sha != row.TempSHA {
		return OutcomeAdvanced, fmt.Errorf("published %s has checksum %s, expected %s", row.NewPath, sha, row.TempSHA)
	}
	if err := p.removeTemps(row); err != nil {
		return OutcomeAdvanced, err
	}
	if err := p.at("place:temp_removed"); err != nil {
		return OutcomeAdvanced, err
	}
	if err := p.recordPublished(ctx, row, sha); err != nil {
		return OutcomeAdvanced, err
	}
	if err := p.at("place:recorded"); err != nil {
		return OutcomeAdvanced, err
	}
	return OutcomeAdvanced, nil
}

// quarantineIncumbent is phase 3 of a swap or undo: copy the incumbent into quarantine and
// record it once the copy's checksum matches the incumbent's recorded one. A destination
// that no longer holds the incumbent stops the item (KTD18).
func (p *Placement) quarantineIncumbent(ctx context.Context, row JournalRow) (Outcome, JournalRow, error) {
	cur, err := placeSHA256(row.NewPath)
	if err != nil {
		return OutcomeAdvanced, row, fmt.Errorf("checksumming incumbent %s: %w", row.NewPath, err)
	}
	if cur != row.IncumbentSHA {
		out, serr := p.stop(ctx, row, "collision", fmt.Sprintf("%s no longer holds the incumbent recorded for this swap; nothing changed", row.NewPath))
		return out, row, serr
	}
	if err := p.copyToQuarantine(row); err != nil {
		return OutcomeAdvanced, row, err
	}
	if err := p.at("quarantine:copied"); err != nil {
		return OutcomeAdvanced, row, err
	}
	fi, err := os.Stat(row.QuarantinePath)
	if err != nil {
		return OutcomeAdvanced, row, err
	}
	err = store.WithTx(ctx, p.m.DB(), func(tx *sql.Tx) error {
		qid, err := journalQuarantineRow(ctx, tx, row.TrackID, row.QuarantinePath, row.NewPath, cur, fi.Size(), p.m.Now())
		if err != nil {
			return err
		}
		_, err = p.jr.Record(ctx, tx, row.ID, PhaseQuarantineVerified, PhaseData{QuarantineSHA: cur, QuarantineID: qid})
		return err
	})
	if err != nil {
		return OutcomeAdvanced, row, err
	}
	if err := p.at("quarantine:recorded"); err != nil {
		return OutcomeAdvanced, row, err
	}
	row, err = p.jr.Get(ctx, row.ID)
	return OutcomeAdvanced, row, err
}

// copyToQuarantine copies the incumbent into the quarantine root and verifies the copy. It
// overwrites a partial copy from an earlier attempt. The quarantine root is never created.
func (p *Placement) copyToQuarantine(row JournalRow) error {
	root := p.cfg.QuarantineRoot
	if root == "" {
		return errors.New("musicengine placement: no quarantine root configured")
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return fmt.Errorf("musicengine placement: quarantine root %s is unreachable: %v", root, err)
	}
	if err := layoutInside(root, row.QuarantinePath); err != nil {
		return err
	}
	dir := filepath.Dir(row.QuarantinePath)
	if err := os.MkdirAll(dir, p.cfg.DirMode); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	if err := p.placeCopy(row.NewPath, row.QuarantinePath, "quarantine:mid_copy"); err != nil {
		return fmt.Errorf("copying incumbent to quarantine: %w", err)
	}
	if _, err := p.placePerms(row.QuarantinePath, false); err != nil {
		return err
	}
	sha, err := placeSHA256(row.QuarantinePath)
	if err != nil {
		return fmt.Errorf("checksumming quarantine copy: %w", err)
	}
	if sha != row.IncumbentSHA {
		return fmt.Errorf("quarantine copy %s has checksum %s, expected %s", row.QuarantinePath, sha, row.IncumbentSHA)
	}
	return nil
}

// publishReplace is phase 4 of a swap or undo: prove the destination still holds the
// incumbent and the quarantine copy is intact, then rename the verified temp file over it.
func (p *Placement) publishReplace(ctx context.Context, row JournalRow) (Outcome, error) {
	cur, err := placeSHA256(row.NewPath)
	if err != nil {
		return OutcomeAdvanced, fmt.Errorf("checksumming %s: %w", row.NewPath, err)
	}
	switch cur {
	case row.TempSHA: // renamed before the crash, never recorded
		return p.finishPublished(ctx, row)
	case row.IncumbentSHA:
	default:
		return p.stop(ctx, row, "collision", fmt.Sprintf("%s no longer holds the incumbent recorded for this swap; nothing changed", row.NewPath))
	}
	if q, qerr := placeSHA256(row.QuarantinePath); qerr != nil || q != row.QuarantineSHA {
		if err := p.copyToQuarantine(row); err != nil { // the incumbent is still here, so redo phase 3's copy
			return OutcomeAdvanced, err
		}
	}
	t, err := placeSHA256(row.TempPath)
	if err != nil || t != row.TempSHA {
		return p.rollback(ctx, row, "the verified temp file is gone or changed; the incumbent was not touched")
	}
	if err := os.Rename(row.TempPath, row.NewPath); err != nil {
		return OutcomeAdvanced, fmt.Errorf("replacing %s: %w", row.NewPath, err)
	}
	if err := p.at("swap:renamed"); err != nil {
		return OutcomeAdvanced, err
	}
	return p.finishPublished(ctx, row)
}

// ---- placing ----

type placeCand struct {
	ID, ItemID        int64
	StagingPath       string
	SHA256            string
	Fingerprint       string
	ReleaseMBID       string
	Verdict           string
	Codec, Container  string
	BitDepth, Rate    int
	Channels, Bitrate int
	DurationMs        int64
	Loudness          sql.NullFloat64
	TopHz             int
	FloorDB           sql.NullFloat64
}

func placeChosen(ctx context.Context, q store.Execer, itemID int64) (placeCand, bool, error) {
	var c placeCand
	err := q.QueryRowContext(ctx,
		`SELECT id, item_id, staging_path, sha256, pcm_fingerprint, release_mbid, verdict, codec, container,
		        bit_depth, sample_rate, channels, bitrate, duration_ms, loudness_lufs, spectral_top_hz, spectral_floor_db
		 FROM mus_candidates WHERE item_id = ? AND state = 'chosen' ORDER BY id DESC LIMIT 1`, itemID).
		Scan(&c.ID, &c.ItemID, &c.StagingPath, &c.SHA256, &c.Fingerprint, &c.ReleaseMBID, &c.Verdict, &c.Codec,
			&c.Container, &c.BitDepth, &c.Rate, &c.Channels, &c.Bitrate, &c.DurationMs, &c.Loudness, &c.TopHz, &c.FloorDB)
	if errors.Is(err, sql.ErrNoRows) {
		return placeCand{}, false, nil
	}
	if err != nil {
		return placeCand{}, false, fmt.Errorf("reading the chosen candidate of item %d: %w", itemID, err)
	}
	return c, true, nil
}

// readTags reads a file's tags: metaflac for FLAC, the engine's TagReader for the rest.
func (p *Placement) readTags(ctx context.Context, path string) (TrackTags, error) {
	if strings.EqualFold(filepath.Ext(path), ".flac") && p.cfg.Editor != nil {
		return p.cfg.Editor.Read(ctx, path)
	}
	if p.cfg.Tags == nil {
		return TrackTags{}, fmt.Errorf("no tag reader for %s", filepath.Ext(path))
	}
	t, err := p.cfg.Tags.ReadTags(ctx, path)
	if err != nil {
		return TrackTags{}, err
	}
	return TrackTags{Artist: t.Artist, Album: t.Album, Title: t.Title, RecordingMBID: t.RecordingMBID}, nil
}

func (t TrackTags) folderArtist() string {
	if strings.TrimSpace(t.AlbumArtist) != "" {
		return t.AlbumArtist
	}
	return t.Artist
}

func (p *Placement) stepPlacing(ctx context.Context, it Item) error {
	if row, ok, err := p.jr.OpenForItem(ctx, it.ID); err != nil {
		return err
	} else if ok {
		out, err := p.Advance(ctx, row)
		if err != nil {
			return err
		}
		if out != OutcomeRolledBack {
			return nil // advanced to published (item is now awaiting Plex) or stopped for Blair
		}
		// rolled back: start a fresh row below
	}
	cand, ok, err := placeChosen(ctx, p.m.DB(), it.ID)
	if err != nil {
		return err
	}
	if !ok {
		return p.m.Transition(ctx, it.ID, StateSearching, "placement found no chosen candidate")
	}
	if err := p.layout.Check(); err != nil {
		return err // storage unreachable: the item stays in placing with this reason
	}
	if fi, err := os.Stat(cand.StagingPath); err != nil || !fi.Mode().IsRegular() {
		return p.rejectAndSearch(ctx, it, cand, fmt.Sprintf("the staged file is gone: %v", err))
	}
	spec, err := p.specForCandidate(ctx, it, cand)
	if err != nil {
		return err
	}
	row, err := p.Stage(ctx, spec)
	if err != nil {
		var ve *VerifyError
		if errors.As(err, &ve) {
			return p.rejectAndSearch(ctx, it, cand, ve.Reason)
		}
		return err
	}
	_, err = p.Advance(ctx, row)
	return err
}

// rejectAndSearch marks the candidate rejected and sends the item back to searching.
func (p *Placement) rejectAndSearch(ctx context.Context, it Item, c placeCand, reason string) error {
	return store.WithTx(ctx, p.m.DB(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE mus_candidates SET state = 'rejected', reject_reason = ? WHERE id = ?`, clip(reason, 500), c.ID); err != nil {
			return fmt.Errorf("rejecting candidate %d: %w", c.ID, err)
		}
		return p.m.TransitionTx(ctx, tx, it.ID, StateSearching, "candidate failed placement: "+clip(reason, 200))
	})
}

// specForCandidate decides where a candidate's file goes and how it is numbered.
func (p *Placement) specForCandidate(ctx context.Context, it Item, c placeCand) (StageSpec, error) {
	rec := Recording{}
	if it.RecordingID != 0 {
		var err error
		if rec, err = getRecording(ctx, p.m.DB(), it.RecordingID); err != nil {
			return StageSpec{}, err
		}
	}
	tags, err := p.readTags(ctx, c.StagingPath)
	if err != nil {
		return StageSpec{}, fmt.Errorf("reading tags of %s: %w", c.StagingPath, err)
	}
	title, artist := firstNonEmpty(tags.Title, rec.Title), firstNonEmpty(tags.folderArtist(), rec.Artist)
	self := layoutTrack{Key: c.StagingPath, Disc: tags.Disc, Track: tags.Track}
	if self.Track < 1 {
		self = p.positionFromRelease(ctx, c, rec, self)
	}
	num, repaired := layoutNumberFor(self, p.siblings(ctx, it, c))
	plan, err := p.layout.Plan(artist, tags.Album, num.Continuous, title, filepath.Ext(c.StagingPath))
	if err != nil {
		return StageSpec{}, err
	}
	if repaired {
		p.log.Info("musicengine placement: track numbering was not usable; fell back", "item", it.ID, "disc", num.Disc, "track", num.Continuous)
	}
	return StageSpec{
		Kind: JournalPlace, ItemID: it.ID, CandidateID: c.ID, RecordingID: it.RecordingID,
		Source: c.StagingPath, SourceSHA: c.SHA256, Dest: plan.Path, ExpectFingerprint: c.Fingerprint,
		Number: &num,
	}, nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// positionFromRelease asks MusicBrainz for the track's disc and position when the file's own
// tags have none. A YouTube candidate has no release and ends up as disc 1 track 1.
func (p *Placement) positionFromRelease(ctx context.Context, c placeCand, rec Recording, self layoutTrack) layoutTrack {
	if p.cfg.Releases == nil || c.ReleaseMBID == "" || rec.MBID == "" {
		return self
	}
	rel, err := p.cfg.Releases.GetRelease(ctx, c.ReleaseMBID)
	if err != nil {
		p.log.Warn("musicengine placement: release lookup failed; numbering falls back", "release", c.ReleaseMBID, "err", err)
		return self
	}
	for _, t := range rel.Tracks {
		if t.RecordingMBID == rec.MBID {
			self.Disc, self.Track = t.Disc, t.Position
			return self
		}
	}
	return self
}

// siblings are the other audio files of the candidate's release in staging, with their tagged
// positions, so a disc-2 track is numbered after the length of disc 1.
func (p *Placement) siblings(ctx context.Context, it Item, c placeCand) []layoutTrack {
	root := filepath.Dir(c.StagingPath)
	if p.cfg.Staging != nil {
		root = p.cfg.Staging.CandidateDir(it.ID, c.ID)
	}
	var out []layoutTrack
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path == c.StagingPath || !stagingAudioExts[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		if t, err := p.readTags(ctx, path); err == nil {
			out = append(out, layoutTrack{Key: path, Disc: t.Disc, Track: t.Track})
		}
		return nil
	})
	return out
}

// ---- awaiting Plex ----

// plexIndexed refreshes the track's folder and polls Plex until the file is listed under the
// artist and album its tags name. It returns Plex's rating key for the track.
func (p *Placement) plexIndexed(ctx context.Context, row JournalRow, tags TrackTags, poll bool) (string, bool, error) {
	section := p.cfg.PlexSection()
	if section == "" {
		return "", false, errors.New("no Plex music section is configured")
	}
	plexPath := p.paths.toPlex(row.NewPath)
	if err := p.cfg.Plex.RefreshPath(ctx, section, filepath.Dir(plexPath)); err != nil {
		return "", false, fmt.Errorf("asking Plex to refresh %s: %w", filepath.Dir(plexPath), err)
	}
	attempts := 1
	if poll {
		attempts = p.cfg.PollAttempts
	}
	for i := 0; i < attempts; i++ {
		if i > 0 {
			p.cfg.Sleep(ctx, p.cfg.PollInterval)
			if ctx.Err() != nil {
				return "", false, ctx.Err()
			}
		}
		tracks, err := p.cfg.Plex.SearchTracks(ctx, section, tags.folderArtist(), tags.Title)
		if err != nil {
			return "", false, fmt.Errorf("searching Plex for %q: %w", tags.Title, err)
		}
		for _, t := range tracks {
			if !strings.EqualFold(t.Album, tags.Album) {
				continue
			}
			for _, f := range t.Files {
				if f == plexPath {
					return t.RatingKey, true, nil
				}
			}
		}
	}
	return "", false, nil
}

func (p *Placement) stepAwaiting(ctx context.Context, it Item) error {
	row, ok, err := p.jr.OpenForItem(ctx, it.ID)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("awaiting Plex but the item has no open journal row")
	}
	if row.Phase < PhasePublished {
		out, err := p.Advance(ctx, row)
		if err != nil || out != OutcomeAdvanced {
			return err
		}
		if row, err = p.jr.Get(ctx, row.ID); err != nil {
			return err
		}
		if row.Phase < PhasePublished {
			return nil
		}
	}
	tags, err := p.readTags(ctx, row.NewPath)
	if err != nil {
		return fmt.Errorf("reading tags of %s: %w", row.NewPath, err)
	}
	key, indexed, err := p.plexIndexed(ctx, row, tags, true)
	if err != nil {
		return err
	}
	if !indexed {
		_, err := p.m.DB().ExecContext(ctx, `UPDATE mus_items SET next_attempt_at = ?, last_reason = ?, updated_at = ? WHERE id = ? AND state = ?`,
			p.m.Now().Add(p.cfg.RetryAfter).Unix(), "waiting for Plex to index "+filepath.Base(row.NewPath), p.m.Now().Unix(), it.ID, string(it.State))
		return err
	}
	if err := p.confirm(ctx, row, tags, key, p.candidateFacts(ctx, row), true); err != nil {
		return err
	}
	if p.cfg.Mirror != nil {
		if _, err := p.cfg.Mirror.Sync(ctx); err != nil {
			// Not fatal for the placement; the next mirror sync repairs it, and the reason is logged.
			p.log.Warn("musicengine placement: mirror sync after placement failed", "item", it.ID, "err", err)
		}
	}
	return nil
}

// placeFacts are the file facts stored on the library track (R16).
type placeFacts struct {
	Codec, Container               string
	BitDepth, SampleRate, Channels int
	Bitrate                        int
	DurationMs                     int64
	Verdict                        string
	TopHz                          int
	FloorDB, LoudnessLUFS          sql.NullFloat64
	Fingerprint, SHA256            string
}

func (p *Placement) candidateFacts(ctx context.Context, row JournalRow) *placeFacts {
	if row.CandidateID == 0 {
		return nil
	}
	var f placeFacts
	err := p.m.DB().QueryRowContext(ctx,
		`SELECT codec, container, bit_depth, sample_rate, channels, bitrate, duration_ms, verdict,
		        spectral_top_hz, spectral_floor_db, loudness_lufs, pcm_fingerprint
		 FROM mus_candidates WHERE id = ?`, row.CandidateID).
		Scan(&f.Codec, &f.Container, &f.BitDepth, &f.SampleRate, &f.Channels, &f.Bitrate, &f.DurationMs, &f.Verdict,
			&f.TopHz, &f.FloorDB, &f.LoudnessLUFS, &f.Fingerprint)
	if err != nil {
		p.log.Warn("musicengine placement: could not read candidate facts; the catalogue will learn the file", "candidate", row.CandidateID, "err", err)
		return nil
	}
	f.SHA256 = row.PublishedSHA
	return &f
}

// confirm records plex_confirmed and closes the row, together with the library track, the
// durable identity and (for a row with an item) the move to done, in one transaction.
func (p *Placement) confirm(ctx context.Context, row JournalRow, tags TrackTags, ratingKey string, facts *placeFacts, finishItem bool) error {
	if row.Kind != JournalPlace && p.cfg.Finalize != nil {
		if err := p.cfg.Finalize(ctx, row, ratingKey); err != nil {
			return fmt.Errorf("finishing %s on Plex: %w", row.Kind, err)
		}
	}
	plexPath := p.paths.toPlex(row.NewPath)
	return store.WithTx(ctx, p.m.DB(), func(tx *sql.Tx) error {
		trackID, err := p.upsertLibraryTrack(ctx, tx, row, tags, ratingKey, plexPath, facts)
		if err != nil {
			return err
		}
		if row.RecordingID != 0 {
			if _, err := RecordIdentity(ctx, tx, IdentityRecord{
				RecordingID: row.RecordingID, Path: plexPath, SHA256: row.PublishedSHA, RatingKey: ratingKey,
			}, p.m.Now()); err != nil {
				return err
			}
		}
		if _, err := p.jr.Record(ctx, tx, row.ID, PhasePlexConfirmed, PhaseData{}); err != nil {
			return err
		}
		if err := p.jr.Close(ctx, tx, row.ID, ""); err != nil {
			return err
		}
		if row.ItemID == 0 || !finishItem {
			return nil
		}
		if trackID != 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE mus_items SET track_id = ? WHERE id = ?`, trackID, row.ItemID); err != nil {
				return fmt.Errorf("linking item %d to library track %d: %w", row.ItemID, trackID, err)
			}
		}
		return p.m.TransitionTx(ctx, tx, row.ItemID, StateDone, "placed and indexed by Plex: "+filepath.Base(row.NewPath))
	})
}

func (p *Placement) upsertLibraryTrack(ctx context.Context, tx store.Execer, row JournalRow, tags TrackTags, ratingKey, plexPath string, f *placeFacts) (int64, error) {
	now := p.m.Now().Unix()
	var rg any
	var explicit int
	if row.RecordingID != 0 {
		var g string
		if err := tx.QueryRowContext(ctx, `SELECT release_group_mbid, explicit FROM mus_recordings WHERE id = ?`, row.RecordingID).Scan(&g, &explicit); err == nil {
			rg = g
		}
	}
	if f == nil {
		f = &placeFacts{SHA256: row.PublishedSHA}
	}
	var id int64
	var err error
	if row.TrackID != 0 {
		err = tx.QueryRowContext(ctx,
			`UPDATE mus_library_tracks SET plex_rating_key = ?, path = ?, title = ?, artist = ?, album = ?,
			        codec = ?, container = ?, bit_depth = ?, sample_rate = ?, channels = ?, bitrate = ?,
			        duration_ms = ?, verdict = ?, verdict_at = ?, spectral_top_hz = ?, spectral_floor_db = ?,
			        loudness_lufs = ?, pcm_fingerprint = ?, sha256 = ?, last_seen_at = ?
			 WHERE id = ? RETURNING id`,
			ratingKey, plexPath, tags.Title, tags.folderArtist(), tags.Album, f.Codec, f.Container, f.BitDepth,
			f.SampleRate, f.Channels, f.Bitrate, f.DurationMs, f.Verdict, now, f.TopHz, f.FloorDB, f.LoudnessLUFS,
			f.Fingerprint, f.SHA256, now, row.TrackID).Scan(&id)
	} else {
		err = tx.QueryRowContext(ctx,
			`INSERT INTO mus_library_tracks (plex_rating_key, path, recording_id, title, artist, album, release_group_mbid,
			        explicit, codec, container, bit_depth, sample_rate, channels, bitrate, duration_ms, verdict, verdict_at,
			        spectral_top_hz, spectral_floor_db, loudness_lufs, pcm_fingerprint, sha256, last_seen_at, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, COALESCE(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(plex_rating_key) DO UPDATE SET path = excluded.path,
			        recording_id = COALESCE(excluded.recording_id, mus_library_tracks.recording_id),
			        title = excluded.title, artist = excluded.artist, album = excluded.album,
			        codec = excluded.codec, container = excluded.container, bit_depth = excluded.bit_depth,
			        sample_rate = excluded.sample_rate, channels = excluded.channels, bitrate = excluded.bitrate,
			        duration_ms = excluded.duration_ms, verdict = excluded.verdict, verdict_at = excluded.verdict_at,
			        spectral_top_hz = excluded.spectral_top_hz, spectral_floor_db = excluded.spectral_floor_db,
			        loudness_lufs = excluded.loudness_lufs, pcm_fingerprint = excluded.pcm_fingerprint,
			        sha256 = excluded.sha256, last_seen_at = excluded.last_seen_at
			 RETURNING id`,
			ratingKey, plexPath, nullID(row.RecordingID), tags.Title, tags.folderArtist(), tags.Album, rg, explicit,
			f.Codec, f.Container, f.BitDepth, f.SampleRate, f.Channels, f.Bitrate, f.DurationMs, f.Verdict, now,
			f.TopHz, f.FloorDB, f.LoudnessLUFS, f.Fingerprint, f.SHA256, now, now).Scan(&id)
	}
	if err != nil {
		return 0, fmt.Errorf("recording library track for %s: %w", plexPath, err)
	}
	return id, nil
}

// ---- path mapping ----

// pathMap translates between this process's library paths and the Plex server's.
type pathMap struct{ local, plex string }

func (m pathMap) swap(p, from, to string) string {
	if from == to {
		return p
	}
	rel, err := filepath.Rel(from, filepath.Clean(p))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return filepath.Join(to, rel)
}

func (m pathMap) toPlex(p string) string  { return m.swap(p, m.local, m.plex) }
func (m pathMap) toLocal(p string) string { return m.swap(p, m.plex, m.local) }
