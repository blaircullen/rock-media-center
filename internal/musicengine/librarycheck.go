package musicengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tristenlammi/arrmada/internal/audiocheck"
	"github.com/tristenlammi/arrmada/internal/metadata"
	"github.com/tristenlammi/arrmada/internal/store"
)

// The library check (R7, KTD6): before anything downloads, look for the same recording in
// the library. Plex search by artist and title finds candidate files; each file's tags
// (MusicBrainz recording id, ISRC) say whether it is the pinned recording, with a
// MusicBrainz album-and-duration comparison when the tags have no ids. The matched file is
// run through the audio checker: a genuine lossless file ends the request, a flawed or
// lossy one makes it an upgrade, an unreadable one is retried (R14).

const (
	// maxBlindAttempts is how many unreadable library reads the item retries before the
	// file goes to Blair as a recovery decision (R14).
	maxBlindAttempts = 5
	blindRetryBase   = 5 * time.Minute
	blindRetryMax    = 6 * time.Hour
	// maxLibraryCandidates and maxMatchedChecks bound the files read and fully decoded for
	// one request; a library holding more copies than that goes to Blair.
	maxLibraryCandidates = 12
	maxMatchedChecks     = 5
)

type matchResult int

const (
	matchNo matchResult = iota
	matchYes
	matchUnsettled // looks like it but the evidence is not enough
)

type libCand struct {
	path      string
	ratingKey string
	dbID      int64 // mus_library_tracks id when known
	known     bool  // already tied to this recording or item; no tag check needed
	title     string
	artist    string
	album     string
}

type checkedFile struct {
	cand libCand
	res  audiocheck.Result
}

// isLossless reports whether an ffprobe codec name is a lossless one. It is the one list:
// acquisition, placement and the library check all use it, and an unknown name is not lossless.
func isLossless(codec string) bool {
	codec = strings.ToLower(strings.TrimSpace(codec))
	if strings.HasPrefix(codec, "pcm_") || strings.HasPrefix(codec, "dsd_") {
		return true
	}
	switch codec {
	case "flac", "alac", "ape", "wavpack", "tta", "mlp", "truehd", "shorten", "tak":
		return true
	}
	return false
}

// libraryFinding is what looking for a recording in the library found. truncated says the
// search stopped at a limit (too many candidate files, or more than maxMatchedChecks
// matches) before every candidate was looked at, so "no adequate copy" cannot be trusted.
type libraryFinding struct {
	matched    int
	winner     *checkedFile // a genuine lossless copy
	flawed     *checkedFile // first matched copy that is lossy or flagged
	unsettled  []libCand
	unreadable string // why a candidate could not be looked at ("" when all could)
	blind      string // why a matched copy could not be checked ("" when all could)
	truncated  bool
}

// lookupLibrary is the one library search: candidates, tag classification, audio check. The
// request flow and full-album placement both go through it.
func (e *Engine) lookupLibrary(ctx context.Context, it Item, rec Recording) (libraryFinding, error) {
	var f libraryFinding
	if e.plex == nil || e.checker == nil || e.tags == nil || e.mb == nil {
		return f, errors.New("musicengine: library check needs Plex, MusicBrainz, a tag reader and the audio checker")
	}
	section := e.settings().PlexSection
	if section == "" {
		return f, errors.New("musicengine: no Plex music section configured")
	}
	mctx := metadata.WithMBClass(ctx, mbClass(it))

	cands, truncated, err := e.libraryCandidates(ctx, section, it, rec)
	if err != nil {
		return f, err
	}
	f.truncated = truncated

	var matched []libCand
	var pinnedFull *metadata.Recording // fetched once, only if a fallback needs release titles
	for _, c := range cands {
		if len(matched) >= maxMatchedChecks {
			f.truncated = true // candidates i.. were never classified
			break
		}
		if c.known {
			matched = append(matched, c)
			continue
		}
		res, why, err := e.classifyFile(mctx, rec, c, &pinnedFull)
		if err != nil {
			return f, err
		}
		switch res {
		case matchYes:
			matched = append(matched, c)
		case matchUnsettled:
			f.unsettled = append(f.unsettled, c)
		default:
			if why != "" && strings.HasPrefix(why, libUnreadablePrefix) {
				f.unreadable = why
			}
		}
	}
	f.matched = len(matched)

	for _, c := range matched {
		res := e.checker.Check(ctx, c.path)
		cf := &checkedFile{cand: c, res: res}
		switch {
		case res.Verdict == audiocheck.Blind:
			f.blind = fmt.Sprintf("Couldn't read your library file %s: %s", c.path, res.Reason)
		case res.Verdict == audiocheck.Genuine && isLossless(res.Facts.Codec):
			f.winner = cf
		default:
			if f.flawed == nil {
				f.flawed = cf
			}
		}
		if f.winner != nil {
			break
		}
	}
	return f, nil
}

// truncatedReason is the text of the ambiguity decision raised when the search was cut short.
func truncatedReason(rec Recording) string {
	return fmt.Sprintf("The search of your music library for %s - %s stopped at its limit (%d files looked at, %d matches) before every copy was checked, so a 'missing' or 'lossy' result can't be trusted.",
		rec.Artist, rec.Title, maxLibraryCandidates, maxMatchedChecks)
}

func (e *Engine) handleLibraryCheck(ctx context.Context, it Item) error {
	if it.RecordingID == 0 {
		return fmt.Errorf("item %d reached library_check with no pinned recording", it.ID)
	}
	rec, err := getRecording(ctx, e.db, it.RecordingID)
	if err != nil {
		return err
	}
	f, err := e.lookupLibrary(ctx, it, rec)
	if err != nil {
		return err
	}

	// Only a positively found adequate copy ends the request; a search that hit a limit
	// without finding one never authorizes a download (Astra 23).
	noMatchSeen := f.matched == 0 && f.unreadable == "" && len(f.unsettled) == 0
	if f.winner == nil && f.truncated && (f.flawed != nil || noMatchSeen) {
		return e.raiseAndWait(ctx, it, "ambiguous", truncatedReason(rec),
			[]string{OptionNotAMatch, OptionDrop})
	}

	if f.matched == 0 {
		switch {
		case f.unreadable != "":
			// A file that could not be read might be the copy; calling the song missing
			// would download a duplicate.
			return e.retryLibraryCheck(ctx, it, f.unreadable)
		case len(f.unsettled) > 0:
			return e.raiseAndWait(ctx, it, "ambiguous",
				fmt.Sprintf("Your library file %s may be %s - %s, but its tags can't settle it.", f.unsettled[0].path, rec.Artist, rec.Title),
				[]string{OptionTreatAsMatch, OptionNotAMatch, OptionDrop})
		}
		return e.Transition(ctx, it.ID, StateSearching, "The song isn't in your library yet.")
	}

	switch {
	case f.winner != nil:
		return e.finishFromLibrary(ctx, it, rec, *f.winner, StateDone, "",
			fmt.Sprintf("Already in your library as %s (%s)", f.winner.cand.path, describeFacts(f.winner.res)))
	case f.flawed != nil:
		flawed := f.flawed
		why := fmt.Sprintf("The copy in your library, %s, is rated %s (%s), so a better one will be looked for.", flawed.cand.path, flawed.res.Verdict, flawed.res.Reason)
		if flawed.res.Verdict == audiocheck.Genuine {
			why = fmt.Sprintf("The copy in your library, %s, is lossy (%s), so a lossless one will be looked for.", flawed.cand.path, flawed.res.Facts.Codec)
		}
		upgrade := ""
		if it.Kind == KindRequest {
			upgrade = KindUpgrade
		}
		return e.finishFromLibrary(ctx, it, rec, *flawed, StateSearching, upgrade, why)
	}
	return e.retryLibraryCheck(ctx, it, f.blind)
}

// TrackBlocked says why a full-album track must not be placed: a copy of the same recording
// is already in the library (adequate, or lossy/flagged, which is a swap and not a second
// file), another item already owns the recording, or the search could not settle it. ""
// means the track is free to place. Unreadable results block too: placing beside a copy that
// could not be looked at would risk a duplicate.
func (e *Engine) TrackBlocked(ctx context.Context, rec Recording) (string, error) {
	if rec.ID != 0 {
		var holder int64
		err := e.db.QueryRowContext(ctx,
			`SELECT id FROM mus_items WHERE recording_id = ? AND state NOT IN ('done','cancelled','dropped') LIMIT 1`,
			rec.ID).Scan(&holder)
		switch {
		case err == nil:
			return fmt.Sprintf("Item %d is already working on this song.", holder), nil
		case !errors.Is(err, sql.ErrNoRows):
			return "", fmt.Errorf("looking for an active item on recording %d: %w", rec.ID, err)
		}
	}
	f, err := e.lookupLibrary(ctx, Item{}, rec)
	if err != nil {
		return "", err
	}
	switch {
	case f.winner != nil:
		return fmt.Sprintf("Already in your library as %s (%s)", f.winner.cand.path, describeFacts(f.winner.res)), nil
	case f.flawed != nil:
		// Placing beside it would leave two copies. Turning it into a swap is a recorded plan
		// deferral (the album flow has no item to swap for).
		return fmt.Sprintf("Your library already has a lossy or flagged copy, %s (%s), so this track was not added next to it. Replacing it is not done for album downloads yet.",
			f.flawed.cand.path, f.flawed.res.Verdict), nil
	case f.blind != "":
		return "Couldn't check the copy in your library: " + f.blind, nil
	case f.unreadable != "":
		return f.unreadable, nil
	case len(f.unsettled) > 0:
		return fmt.Sprintf("Your library file %s may be this song, but its tags can't settle it.", f.unsettled[0].path), nil
	case f.truncated:
		return truncatedReason(rec), nil
	}
	return "", nil
}

func describeFacts(r audiocheck.Result) string {
	f := r.Facts
	if f.BitDepth > 0 {
		return fmt.Sprintf("%s %d-bit/%d Hz, %s", f.Codec, f.BitDepth, f.SampleRate, r.Verdict)
	}
	return fmt.Sprintf("%s %.0f kbps, %s", f.Codec, f.BitrateKbps, r.Verdict)
}

// finishFromLibrary records the checked library file and moves the item on in one
// transaction: the file's facts and verdict on its library row, the row tied to the
// recording and to the item (the note the mirror reads), and the state change.
func (e *Engine) finishFromLibrary(ctx context.Context, it Item, rec Recording, cf checkedFile, to State, newKind, reason string) error {
	now := e.now()
	return store.WithTx(ctx, e.db, func(tx *sql.Tx) error {
		trackID, err := persistCheck(ctx, tx, cf.cand, rec.ID, cf.res, now)
		if err != nil {
			return err
		}
		kind := it.Kind
		if newKind != "" {
			kind = newKind
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE mus_items SET track_id = ?, kind = ?, updated_at = ? WHERE id = ?`, trackID, kind, now.Unix(), it.ID); err != nil {
			return fmt.Errorf("linking item %d to library track %d: %w", it.ID, trackID, err)
		}
		return e.TransitionTx(ctx, tx, it.ID, to, reason)
	})
}

func persistCheck(ctx context.Context, q store.Execer, c libCand, recordingID int64, r audiocheck.Result, now time.Time) (int64, error) {
	id := c.dbID
	if id == 0 {
		if c.ratingKey == "" {
			return 0, fmt.Errorf("library file %s has no Plex rating key", c.path)
		}
		err := q.QueryRowContext(ctx,
			`INSERT INTO mus_library_tracks (plex_rating_key, path, title, artist, album, last_seen_at, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(plex_rating_key) DO UPDATE SET path = excluded.path RETURNING id`,
			c.ratingKey, c.path, c.title, c.artist, c.album, now.Unix(), now.Unix()).Scan(&id)
		if err != nil {
			return 0, fmt.Errorf("storing library track %s: %w", c.path, err)
		}
	}
	var topHz int
	var floor any
	if r.Signature != nil {
		topHz = int(r.Signature.TopHz)
		floor = r.Signature.FloorDB
	}
	var lufs any
	if r.LoudnessLUFS != nil {
		lufs = *r.LoudnessLUFS
	}
	f := r.Facts
	_, err := q.ExecContext(ctx,
		`UPDATE mus_library_tracks SET recording_id = ?, codec = ?, container = ?, bit_depth = ?,
		        sample_rate = ?, channels = ?, bitrate = ?, duration_ms = ?, verdict = ?, verdict_at = ?,
		        spectral_top_hz = ?, spectral_floor_db = ?, loudness_lufs = ?, pcm_fingerprint = ?
		 WHERE id = ?`,
		recordingID, f.Codec, f.Container, f.BitDepth, f.SampleRate, f.Channels,
		int(f.BitrateKbps+0.5), int(f.DurationSec*1000), string(r.Verdict), now.Unix(),
		topHz, floor, lufs, r.Fingerprint, id)
	if err != nil {
		return 0, fmt.Errorf("recording the check of library track %d: %w", id, err)
	}
	return id, nil
}

// retryLibraryCheck keeps the item in library_check and schedules another read, with a
// doubling interval; after maxBlindAttempts the file goes to Blair as a recovery decision
// (R14), never to the upgrade path.
func (e *Engine) retryLibraryCheck(ctx context.Context, it Item, reason string) error {
	cur, err := getItem(ctx, e.db, it.ID)
	if err != nil {
		return err
	}
	attempts := cur.Attempts + 1
	if attempts >= maxBlindAttempts {
		return e.raiseAndWait(ctx, cur, "blind_library",
			fmt.Sprintf("Couldn't read the copy in your library after %d tries: %s", attempts, reason),
			[]string{OptionRetry, OptionDrop})
	}
	interval := time.Duration(cur.RetryIntervalS) * time.Second * 2
	if interval < blindRetryBase {
		interval = blindRetryBase
	}
	if interval > blindRetryMax {
		interval = blindRetryMax
	}
	now := e.now()
	_, err = e.db.ExecContext(ctx,
		`UPDATE mus_items SET attempts = ?, retry_interval_s = ?, next_attempt_at = ?, last_reason = ?, updated_at = ?
		 WHERE id = ? AND state = 'library_check'`,
		attempts, int64(interval/time.Second), now.Add(interval).Unix(),
		"Will try again: "+clip(reason, 400), now.Unix(), it.ID)
	if err != nil {
		return fmt.Errorf("scheduling library check retry for item %d: %w", it.ID, err)
	}
	return nil
}

// libraryCandidates lists the library files that might be the recording: the item's own
// track when the sweep named one, files already tied to the recording, and Plex search
// results for the artist and title as MusicBrainz and Spotify spell them.
func (e *Engine) libraryCandidates(ctx context.Context, section string, it Item, rec Recording) ([]libCand, bool, error) {
	var out []libCand
	truncated := false
	seen := map[string]bool{}
	add := func(c libCand) {
		if c.path == "" || seen[c.path] {
			return
		}
		if len(out) >= maxLibraryCandidates {
			truncated = true // a further distinct file that was not looked at
			return
		}
		seen[c.path] = true
		out = append(out, c)
	}

	q := `SELECT id, plex_rating_key, path, title, artist, album FROM mus_library_tracks WHERE `
	var args []any
	if it.TrackID != 0 {
		q += `id = ?`
		args = append(args, it.TrackID)
	} else {
		q += `recording_id = ? ORDER BY id`
		args = append(args, rec.ID)
	}
	rows, err := e.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, false, fmt.Errorf("reading library tracks for item %d: %w", it.ID, err)
	}
	for rows.Next() {
		var c libCand
		if err := rows.Scan(&c.dbID, &c.ratingKey, &c.path, &c.title, &c.artist, &c.album); err != nil {
			_ = rows.Close()
			return nil, false, fmt.Errorf("scanning library track: %w", err)
		}
		c.known = true
		add(c)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, false, err
	}
	_ = rows.Close()
	if it.TrackID != 0 {
		return out, truncated, nil
	}

	type q2 struct{ artist, title string }
	var searches []q2
	seenQ := map[string]bool{}
	addQ := func(artist, title string) {
		artist, title = firstArtist(artist), strings.TrimSpace(title)
		k := strings.ToLower(artist + "\x00" + title)
		if artist == "" || title == "" || seenQ[k] {
			return
		}
		seenQ[k] = true
		searches = append(searches, q2{artist, title})
	}
	addQ(rec.Artist, rec.Title)
	if en, ok, err := entryForItem(ctx, e.db, it.ID); err != nil {
		return nil, false, err
	} else if ok {
		addQ(en.Artist, en.Title)
	}
	for _, s := range searches {
		tracks, err := e.plex.SearchTracks(ctx, section, s.artist, s.title)
		if err != nil {
			return nil, false, fmt.Errorf("searching Plex for %q - %q: %w", s.artist, s.title, err)
		}
		for _, t := range tracks {
			for _, f := range t.Files {
				add(libCand{path: f, ratingKey: t.RatingKey, title: t.Title, artist: t.Artist, album: t.Album})
			}
		}
	}
	return out, truncated, nil
}

// libUnreadablePrefix starts the reason when a library file could not be looked at.
const libUnreadablePrefix = "Couldn't read the file: "

// classifyFile decides whether one library file is the pinned recording. A why starting
// with libUnreadablePrefix means the file could not be looked at (not that it is a different song).
// An error is a transient MusicBrainz failure and retries the whole check.
func (e *Engine) classifyFile(ctx context.Context, rec Recording, c libCand, pinnedFull **metadata.Recording) (matchResult, string, error) {
	tags, err := e.tags.ReadTags(ctx, c.path)
	if err != nil {
		return matchNo, libUnreadablePrefix + clip(err.Error(), 300), nil
	}

	if tags.RecordingMBID != "" {
		if tags.RecordingMBID == rec.MBID {
			return matchYes, "recording id tag", nil
		}
		other, err := e.mb.GetRecording(ctx, tags.RecordingMBID)
		if err != nil {
			return matchNo, "", fmt.Errorf("musicbrainz recording %s (tag of %s): %w", tags.RecordingMBID, c.path, err)
		}
		if other == nil {
			return matchUnsettled, "tag names a recording MusicBrainz does not return", nil
		}
		if sameRecording(rec, *other) {
			return matchYes, "equivalent recording", nil
		}
		return matchNo, "different recording " + other.MBID, nil
	}
	if tags.ISRC != "" && rec.ISRC != "" && strings.EqualFold(tags.ISRC, rec.ISRC) && fileVersionsMatch(rec, tags, c) {
		return matchYes, "isrc tag", nil
	}

	// No usable ids: compare album and duration against MusicBrainz.
	if !fileVersionsMatch(rec, tags, c) {
		return matchNo, "version differs", nil
	}
	if rec.LengthMs > 0 && tags.DurationSec > 0 && absInt(int(tags.DurationSec*1000)-rec.LengthMs) > durationToleranceMs {
		return matchNo, "duration differs", nil
	}
	title, artist, album := tags.Title, tags.Artist, tags.Album
	if title == "" {
		title = c.title
	}
	if artist == "" {
		artist = c.artist
	}
	if album == "" {
		album = c.album
	}
	if normName(title) != normName(rec.Title) || !artistsOverlap(artist, rec.Artist) {
		return matchNo, "title or artist differs", nil
	}
	if rec.MBID == "" {
		return matchUnsettled, "title, artist and duration agree but the recording has no id to compare releases", nil
	}
	if *pinnedFull == nil {
		full, err := e.mb.GetRecording(ctx, rec.MBID)
		if err != nil {
			return matchNo, "", fmt.Errorf("musicbrainz recording %s: %w", rec.MBID, err)
		}
		if full == nil {
			full = &metadata.Recording{MBID: rec.MBID}
		}
		*pinnedFull = full
	}
	for _, r := range (*pinnedFull).Releases {
		if na := normName(album); na != "" && normName(r.Title) == na {
			return matchYes, "album and duration", nil
		}
	}
	return matchUnsettled, "title, artist and duration agree but the album is not one of the recording's releases", nil
}

// fileVersionsMatch compares the version words in a file's title (and a "live" album)
// against the pinned recording's.
func fileVersionsMatch(rec Recording, tags Tags, c libCand) bool {
	title := tags.Title
	if title == "" {
		title = c.title
	}
	album := tags.Album
	if album == "" {
		album = c.album
	}
	file := versionHints(title)
	for _, h := range versionHints(album) {
		if h == "live" {
			file = unionHints(file, []string{"live"})
		}
	}
	want := versionsFromHint(rec.VersionHint)
	return sameSet(unionHints(file), unionHints(want))
}

// sameRecording: another MusicBrainz recording is the same song as the pinned one when they
// share an ISRC, or have the same title, the same version words, a compatible explicit flag
// and lengths within tolerance. MusicBrainz often holds several recordings of one master.
func sameRecording(rec Recording, o metadata.Recording) bool {
	if rec.ISRC != "" {
		for _, i := range o.ISRCs {
			if strings.EqualFold(i, rec.ISRC) {
				return true
			}
		}
	}
	if normName(o.Title) != normName(rec.Title) {
		return false
	}
	if !sameSet(recordingVersions(o), unionHints(versionsFromHint(rec.VersionHint))) {
		return false
	}
	if !explicitCompatible(o, rec.Explicit) {
		return false
	}
	if rec.LengthMs > 0 && o.LengthMs > 0 && absInt(rec.LengthMs-o.LengthMs) > durationToleranceMs {
		return false
	}
	return true
}
