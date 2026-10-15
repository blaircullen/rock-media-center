package musicengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/tristenlammi/arrmada/internal/metadata"
	"github.com/tristenlammi/arrmada/internal/store"
)

// Pinning (R6, KTD2): resolve the Spotify entry to the one MusicBrainz recording Blair
// heard. The ISRC is the key onto the recording; version hints (live, acoustic, remix, edit,
// demo, instrumental) and the explicit flag must agree with the Spotify entry. A regional
// relink, a local or identity-less entry, and anything that does not resolve to exactly one
// recording goes to Blair once instead of being guessed.

const (
	// maxPinOptions bounds the "use:<mbid>" choices put to Blair.
	maxPinOptions = 5
	// durationTolerance separates two edits of one song. Spotify and MusicBrainz round
	// lengths differently, so this is loose; a different edit is usually 10+ s off.
	durationToleranceMs = 3000
)

var versionWordRe = regexp.MustCompile(`\b(live|acoustic|remix(?:ed)?|edit(?:ed)?|demo|instrumental)\b`)

// versionHints reads version words out of a title or disambiguation. They are hints to
// compare between two descriptions of a song, not a verdict about either.
func versionHints(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range versionWordRe.FindAllString(strings.ToLower(s), -1) {
		switch m {
		case "remixed":
			m = "remix"
		case "edited":
			m = "edit"
		}
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

func unionHints(sets ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, set := range sets {
		for _, h := range set {
			if !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	sort.Strings(out)
	return out
}

// recordingVersions are all the version hints a MusicBrainz recording carries: its title,
// its disambiguation, MusicBrainz's own hints, and "live" when every release it appears on
// is a live release.
func recordingVersions(r metadata.Recording) []string {
	sets := [][]string{versionHints(r.Title), versionHints(r.Disambiguation), r.Versions}
	if len(r.Releases) > 0 {
		allLive := true
		for _, rel := range r.Releases {
			live := false
			for _, v := range rel.Versions {
				if strings.EqualFold(v, "live") {
					live = true
				}
			}
			if !live {
				allLive = false
				break
			}
		}
		if allLive {
			sets = append(sets, []string{"live"})
		}
	}
	return unionHints(sets...)
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func versionHintString(v []string) string {
	if len(v) == 0 {
		return "studio"
	}
	return strings.Join(v, ",")
}

func versionsFromHint(h string) []string {
	if h == "" || h == "studio" {
		return nil
	}
	return strings.Split(h, ",")
}

// explicitHint reads whether MusicBrainz marks the recording explicit (+1), clean (-1) or
// says nothing (0).
func explicitHint(r metadata.Recording) int {
	d := strings.ToLower(r.Disambiguation + " " + r.Title)
	switch {
	case strings.Contains(d, "explicit"):
		return 1
	case strings.Contains(d, "clean"), strings.Contains(d, "censored"):
		return -1
	}
	return 0
}

// explicitCompatible: an explicit Spotify entry never pins to a recording MusicBrainz calls
// clean, and a non-explicit entry never to one it calls explicit (AE8's rule, applied at pin).
func explicitCompatible(r metadata.Recording, wantExplicit bool) bool {
	h := explicitHint(r)
	return !(wantExplicit && h < 0) && !(!wantExplicit && h > 0)
}

var parenRe = regexp.MustCompile(`[\(\[][^\)\]]*[\)\]]`)

// normName reduces a title, artist or album to letters and digits, dropping bracketed
// qualifiers and a " - suffix", so "Song (Remastered)" and "Song - 2011 Remaster" compare equal.
func normName(s string) string {
	s = strings.ToLower(s)
	s = parenRe.ReplaceAllString(s, " ")
	if i := strings.Index(s, " - "); i >= 0 {
		s = s[:i]
	}
	var sb strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

func firstArtist(joined string) string {
	for _, sep := range []string{", ", " feat. ", " ft. ", " & ", " x "} {
		if i := strings.Index(strings.ToLower(joined), sep); i > 0 {
			return joined[:i]
		}
	}
	return joined
}

func artistsOverlap(a, b string) bool {
	na, nb := normName(firstArtist(a)), normName(firstArtist(b))
	if na == "" || nb == "" {
		return false
	}
	return strings.Contains(normName(a), nb) || strings.Contains(normName(b), na) || na == nb
}

func mbClass(it Item) metadata.MBClass {
	if it.Kind == KindSweep {
		return metadata.MBSweep
	}
	return metadata.MBIntake
}

// lookupCandidates finds the recordings the entry could be: by ISRC, else (or when the ISRC
// is unknown to MusicBrainz) by artist, title and duration. searched reports the latter,
// which needs a title and artist cross-check that an ISRC does not.
func (e *Engine) lookupCandidates(ctx context.Context, en entryRow) (cands []metadata.Recording, searched bool, err error) {
	if en.ISRC != "" {
		cands, err = e.mb.RecordingsByISRC(ctx, en.ISRC)
		if err != nil {
			return nil, false, fmt.Errorf("musicbrainz recordings by isrc %s: %w", en.ISRC, err)
		}
		if len(cands) > 0 {
			return cands, false, nil
		}
	}
	cands, err = e.mb.SearchRecordings(ctx, firstArtist(en.Artist), en.Title, en.DurationMs)
	if err != nil {
		return nil, true, fmt.Errorf("musicbrainz search %q / %q: %w", en.Artist, en.Title, err)
	}
	return cands, true, nil
}

// filterCandidates keeps the recordings that agree with the Spotify entry on version,
// explicit flag and (for a search) title, artist and length.
func filterCandidates(en entryRow, cands []metadata.Recording, searched bool) []metadata.Recording {
	want := versionHints(en.Title)
	var out []metadata.Recording
	for _, c := range cands {
		if !sameSet(recordingVersions(c), want) || !explicitCompatible(c, en.Explicit) {
			continue
		}
		if searched {
			if normName(c.Title) != normName(en.Title) || !artistsOverlap(c.Artist, en.Artist) {
				continue
			}
			if c.LengthMs > 0 && en.DurationMs > 0 && absInt(c.LengthMs-en.DurationMs) > durationToleranceMs {
				continue
			}
		}
		out = append(out, c)
	}
	if len(out) > 1 && en.DurationMs > 0 {
		var near []metadata.Recording
		for _, c := range out {
			if c.LengthMs > 0 && absInt(c.LengthMs-en.DurationMs) <= durationToleranceMs {
				near = append(near, c)
			}
		}
		if len(near) > 0 {
			out = near
		}
	}
	return out
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func describeRecording(c metadata.Recording) string {
	s := fmt.Sprintf("%s: %s - %s", c.MBID, c.Artist, c.Title)
	if c.Disambiguation != "" {
		s += " (" + c.Disambiguation + ")"
	}
	if c.LengthMs > 0 {
		s += fmt.Sprintf(" [%ds]", c.LengthMs/1000)
	}
	return s
}

func (e *Engine) handlePinning(ctx context.Context, it Item) error {
	if it.RecordingID != 0 {
		return e.Transition(ctx, it.ID, StateLibraryCheck, "recording already pinned")
	}
	if e.mb == nil {
		return errors.New("musicengine: no MusicBrainz client configured")
	}
	en, ok, err := entryForItem(ctx, e.db, it.ID)
	if err != nil {
		return err
	}
	if !ok {
		return e.raiseAndWait(ctx, it, "ambiguous", "no playlist entry to pin this item to", []string{OptionRetry, OptionDrop})
	}

	mctx := metadata.WithMBClass(ctx, mbClass(it))
	cands, searched, err := e.lookupCandidates(mctx, en)
	if err != nil {
		return err
	}
	filtered := filterCandidates(en, cands, searched)

	var forced string
	switch {
	case en.Unavailable:
		forced = "Spotify lists this track as unavailable"
	case en.IsLocal:
		forced = "local file or an entry with no Spotify track id"
	}
	// A relink (Spotify serving the same song under another track id in this market) is
	// pinned like any entry when exactly one recording fits (Blair, 2026-10-15: "auto-pin
	// relinks"); only an ambiguous relink goes to him, with the relink named in the reason.
	if forced == "" && len(filtered) == 1 {
		return e.applyPin(mctx, it, en, filtered[0])
	}

	reason := forced
	pool := filtered
	switch {
	case forced != "":
	case len(filtered) == 0 && len(cands) == 0:
		reason = "MusicBrainz knows no recording for this song"
	case len(filtered) == 0:
		reason = "no MusicBrainz recording matches the version and explicit flag of the Spotify entry"
		pool = cands
	default:
		reason = fmt.Sprintf("%d MusicBrainz recordings match equally", len(filtered))
	}
	if forced == "" && en.RelinkedID != "" {
		reason += " (Spotify relinked this track, was " + en.RelinkedID + ")"
	}
	if forced != "" && len(pool) == 0 {
		pool = cands
	}
	return e.raiseAndWait(ctx, it, "ambiguous", ambiguousReason(en, reason, pool), pinOptions(pool))
}

func ambiguousReason(en entryRow, reason string, pool []metadata.Recording) string {
	s := fmt.Sprintf("%s - %s: %s", en.Artist, en.Title, reason)
	if len(pool) > maxPinOptions {
		pool = pool[:maxPinOptions]
	}
	for _, c := range pool {
		s += "; candidate " + describeRecording(c)
	}
	return s
}

func pinOptions(pool []metadata.Recording) []string {
	if len(pool) > maxPinOptions {
		pool = pool[:maxPinOptions]
	}
	var opts []string
	for _, c := range pool {
		opts = append(opts, OptionUsePrefix+c.MBID)
	}
	return append(opts, OptionRetry, OptionDrop)
}

// PinToRecording pins an item to the recording Blair chose from an ambiguous-recording
// decision ("use:<mbid>"). The item must be pinning or waiting for Blair.
func (e *Engine) PinToRecording(ctx context.Context, itemID int64, mbid string) error {
	if e.mb == nil {
		return errors.New("musicengine: no MusicBrainz client configured")
	}
	it, err := getItem(ctx, e.db, itemID)
	if err != nil {
		return err
	}
	if it.State != StatePinning && it.State != StateWaitingBlair {
		return fmt.Errorf("%w: item %d is %s, not waiting to be pinned", ErrInvalidTransition, itemID, it.State)
	}
	en, ok, err := entryForItem(ctx, e.db, it.ID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("item %d has no playlist entry to pin", itemID)
	}
	mctx := metadata.WithMBClass(ctx, mbClass(it))
	rec, err := e.mb.GetRecording(mctx, mbid)
	if err != nil {
		return fmt.Errorf("musicbrainz recording %s: %w", mbid, err)
	}
	if rec == nil {
		return fmt.Errorf("musicbrainz returned no recording for %s", mbid)
	}
	return e.applyPin(mctx, it, en, *rec)
}

// pickRelease chooses the release recorded as the recording's home: the one titled like the
// Spotify album when there is one; else the artist's own release (official, a release group
// with no secondary type such as Compilation, Live or Soundtrack), album before EP before
// single, earliest first; else the earliest official non-live release. Without the middle
// step a song landed in "Various Artists/NOW That's What I Call Music" (Beast, 2026-10-15).
func pickRelease(c metadata.Recording, album string) (releaseMBID, groupMBID string) {
	if len(c.Releases) == 0 {
		return "", ""
	}
	if na := normName(album); na != "" {
		for _, r := range c.Releases {
			if normName(r.Title) == na {
				return r.MBID, r.ReleaseGroupMBID
			}
		}
	}
	earlier := func(a, b metadata.ReleaseRef) bool { return a.Date != "" && (b.Date == "" || a.Date < b.Date) }
	own := -1
	for i, r := range c.Releases {
		if !strings.EqualFold(r.Status, "official") || len(r.Versions) > 0 || len(r.SecondaryTypes) > 0 {
			continue
		}
		rank := releaseTypeRank(r.PrimaryType)
		if rank < 0 {
			continue
		}
		if own < 0 {
			own = i
			continue
		}
		cur := releaseTypeRank(c.Releases[own].PrimaryType)
		if rank < cur || (rank == cur && earlier(r, c.Releases[own])) {
			own = i
		}
	}
	if own >= 0 {
		return c.Releases[own].MBID, c.Releases[own].ReleaseGroupMBID
	}
	best := -1
	for i, r := range c.Releases {
		if strings.EqualFold(r.Status, "official") && len(r.Versions) == 0 {
			if best < 0 || earlier(r, c.Releases[best]) {
				best = i
			}
		}
	}
	if best < 0 {
		best = 0
	}
	return c.Releases[best].MBID, c.Releases[best].ReleaseGroupMBID
}

// releaseTypeRank orders a release group's primary type for pickRelease; -1 is not the
// artist's own release.
func releaseTypeRank(primary string) int {
	switch strings.ToLower(primary) {
	case "album":
		return 0
	case "ep":
		return 1
	case "single":
		return 2
	}
	return -1
}

// recordingRow turns a MusicBrainz recording into the stored shape.
func recordingRow(c metadata.Recording, en entryRow) Recording {
	isrc := en.ISRC
	if isrc == "" && len(c.ISRCs) > 0 {
		isrc = c.ISRCs[0]
	}
	explicit := en.Explicit
	switch explicitHint(c) {
	case 1:
		explicit = true
	case -1:
		explicit = false
	}
	rel, grp := pickRelease(c, en.Album)
	return Recording{
		MBID: c.MBID, Title: c.Title, Artist: c.Artist, ISRC: isrc, LengthMs: c.LengthMs,
		Disambiguation: c.Disambiguation, VersionHint: versionHintString(recordingVersions(c)),
		Explicit: explicit, ReleaseGroupMBID: grp, ReleaseMBID: rel,
	}
}

// applyPin records the pin in one transaction. If another active item already holds the
// recording, this item merges into it (KTD2, KTD3): the entry points at the surviving item,
// the survivor takes the higher priority, and this item is cancelled with the reason. The
// item already holding the recording survives because it may be further along.
func (e *Engine) applyPin(ctx context.Context, it Item, en entryRow, c metadata.Recording) error {
	if c.MBID == "" {
		return errors.New("musicbrainz recording has no id")
	}
	if len(c.Releases) == 0 {
		full, err := e.mb.GetRecording(ctx, c.MBID)
		if err != nil {
			return fmt.Errorf("musicbrainz recording %s: %w", c.MBID, err)
		}
		if full != nil {
			c = *full
		}
	}
	row := recordingRow(c, en)
	now := e.now()
	return store.WithTx(ctx, e.db, func(tx *sql.Tx) error {
		cur, err := getItem(ctx, tx, it.ID)
		if err != nil {
			return err
		}
		if cur.State != StatePinning && cur.State != StateWaitingBlair {
			return nil // another run already moved the item on
		}
		recID, err := upsertRecording(ctx, tx, row, now)
		if err != nil {
			return err
		}
		var holder int64
		err = tx.QueryRowContext(ctx,
			`SELECT id FROM mus_items WHERE recording_id = ? AND id <> ? AND state NOT IN ('done','cancelled','dropped') LIMIT 1`,
			recID, it.ID).Scan(&holder)
		switch {
		case err == nil:
			return e.mergeInto(ctx, tx, cur, holder, recID, row.MBID)
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("looking for an active item on recording %s: %w", row.MBID, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE mus_items SET recording_id = ?, updated_at = ? WHERE id = ?`, recID, now.Unix(), it.ID); err != nil {
			return fmt.Errorf("pinning item %d: %w", it.ID, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE mus_playlist_entries SET recording_id = ? WHERE item_id = ?`, recID, it.ID); err != nil {
			return fmt.Errorf("pinning entries of item %d: %w", it.ID, err)
		}
		return e.TransitionTx(ctx, tx, it.ID, StateLibraryCheck,
			fmt.Sprintf("pinned to %s (%s)", row.MBID, row.VersionHint))
	})
}

// mergeInto folds `from` into the holder. A holder that is a sweep item becomes an upgrade
// (Astra 24): it keeps its track and swap path, but an intake request is now attached, so
// the sweep must not close its "no genuine copy" decision as a miss, and it uses the
// intake space policy. It does not become a request: that would place a duplicate.
func (e *Engine) mergeInto(ctx context.Context, tx store.Execer, from Item, holder, recID int64, mbid string) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE mus_playlist_entries SET item_id = ?, recording_id = ? WHERE item_id = ?`, holder, recID, from.ID); err != nil {
		return fmt.Errorf("pointing entries of item %d at item %d: %w", from.ID, holder, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE mus_items SET priority = MAX(priority, ?), updated_at = ?,
		        kind = CASE WHEN kind = ? THEN ? ELSE kind END
		 WHERE id = ?`,
		from.Priority, e.now().Unix(), KindSweep, KindUpgrade, holder); err != nil {
		return fmt.Errorf("raising priority of item %d: %w", holder, err)
	}
	return e.TransitionTx(ctx, tx, from.ID, StateCancelled,
		fmt.Sprintf("merged into item %d: same recording %s", holder, mbid))
}
