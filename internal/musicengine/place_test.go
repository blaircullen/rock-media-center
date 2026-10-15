package musicengine

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tristenlammi/arrmada/internal/audiocheck"
	"github.com/tristenlammi/arrmada/internal/plex"
)

// ---- fakes: a file's tags travel inside its bytes (a trailer), so they survive the copy,
// the hard link and the rename exactly as real tags would ----

const placeTrailer = "\nTAGS:"

func placeEncode(audio string, t TrackTags) []byte {
	b, _ := json.Marshal(t)
	return []byte(audio + placeTrailer + string(b))
}

func placeDecode(b []byte) (string, TrackTags, error) {
	s := string(b)
	i := strings.LastIndex(s, placeTrailer)
	if i < 0 {
		return s, TrackTags{}, errors.New("no tags trailer")
	}
	var t TrackTags
	if err := json.Unmarshal([]byte(s[i+len(placeTrailer):]), &t); err != nil {
		return "", TrackTags{}, err
	}
	return s[:i], t, nil
}

type placeFakeEditor struct {
	mu     sync.Mutex
	writes []string
}

func (e *placeFakeEditor) Read(_ context.Context, path string) (TrackTags, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return TrackTags{}, err
	}
	_, t, err := placeDecode(b)
	return t, err
}

func (e *placeFakeEditor) WriteNumbering(_ context.Context, path string, disc, track int) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	audio, t, err := placeDecode(b)
	if err != nil {
		return err
	}
	t.Disc, t.Track = disc, track
	e.mu.Lock()
	e.writes = append(e.writes, fmt.Sprintf("%s d%d t%d", filepath.Base(path), disc, track))
	e.mu.Unlock()
	return os.WriteFile(path, placeEncode(audio, t), 0o600)
}

// placeFakeChecker judges a file by markers in its audio part.
type placeFakeChecker struct {
	mu    sync.Mutex
	calls map[string]int
}

func placeFP(audio string) string {
	h := sha256.Sum256([]byte(audio))
	return hex.EncodeToString(h[:])
}

func (c *placeFakeChecker) Check(_ context.Context, path string) audiocheck.Result {
	c.mu.Lock()
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[path]++
	c.mu.Unlock()
	b, err := os.ReadFile(path)
	if err != nil {
		return audiocheck.Result{Path: path, Verdict: audiocheck.Blind, Reason: err.Error()}
	}
	audio, _, _ := placeDecode(b)
	r := audiocheck.Result{
		Path: path, Verdict: audiocheck.Genuine, Reason: "ok", Fingerprint: placeFP(audio),
		Facts:     audiocheck.Facts{Codec: "flac", Container: "flac", BitDepth: 16, SampleRate: 44100, Channels: 2, BitrateKbps: 900, DurationSec: 200},
		Integrity: audiocheck.Integrity{OK: true, DecodedSec: 200},
		Signature: &audiocheck.Signature{TopHz: 21000, FloorDB: -70},
	}
	if strings.Contains(audio, "AACCODEC") {
		r.Facts.Codec, r.Facts.Container, r.Facts.BitrateKbps = "aac", "m4a", 256
	}
	switch {
	case strings.Contains(audio, "BLIND"):
		r.Verdict, r.Reason = audiocheck.Blind, "cannot read"
	case strings.Contains(audio, "CORRUPT"):
		r.Verdict, r.Reason, r.Integrity.OK, r.Integrity.Detail = audiocheck.Corrupt, "decoder errors", false, "frame errors"
	case strings.Contains(audio, "FAKE"):
		r.Verdict, r.Reason = audiocheck.Fake, "128k-class shelf"
	case strings.Contains(audio, "HIRES"):
		r.Facts.BitDepth, r.Facts.SampleRate = 24, 96000
	}
	return r
}

// placeFakePlex derives what Plex "indexes" from the files on disk: a track appears after its
// folder was refreshed and lag further searches. Rating keys depend on a salt, so a
// simulated re-import changes every one of them.
type placeFakePlex struct {
	mu        sync.Mutex
	lib       string
	plexRoot  string
	lag       int
	searches  int
	refreshed map[string]bool
	salt      string
	ratings   map[string]float64
	playlists map[string]*placeFakePlaylist
	nextPL    int
	dropAdds  bool
	refreshes []string
}

type placeFakePlaylist struct {
	Title string
	Keys  []string
}

func newPlaceFakePlex(lib, plexRoot string) *placeFakePlex {
	return &placeFakePlex{lib: lib, plexRoot: plexRoot, refreshed: map[string]bool{}, salt: "a",
		ratings: map[string]float64{}, playlists: map[string]*placeFakePlaylist{}}
}

func (f *placeFakePlex) keyFor(plexPath string) string {
	h := sha1.Sum([]byte(f.salt + plexPath))
	return "rk" + hex.EncodeToString(h[:3])
}

func (f *placeFakePlex) toPlex(local string) string {
	rel, _ := filepath.Rel(f.lib, local)
	return filepath.Join(f.plexRoot, rel)
}

func (f *placeFakePlex) scan(onlyRefreshed bool) []plex.Track {
	var out []plex.Track
	_ = filepath.WalkDir(f.lib, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		_, t, derr := placeDecode(b)
		if derr != nil {
			return nil
		}
		if onlyRefreshed && !f.refreshed[f.toPlex(filepath.Dir(p))] {
			return nil
		}
		pp := f.toPlex(p)
		artist := t.AlbumArtist
		if artist == "" {
			artist = t.Artist
		}
		key := f.keyFor(pp)
		out = append(out, plex.Track{RatingKey: key, Title: t.Title, Artist: artist, Album: t.Album, Files: []string{pp}, UserRating: f.ratings[key]})
		return nil
	})
	return out
}

func (f *placeFakePlex) RefreshPath(_ context.Context, _, dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshed[dir] = true
	f.refreshes = append(f.refreshes, dir)
	return nil
}

func (f *placeFakePlex) SearchTracks(_ context.Context, _, artist, title string) ([]plex.Track, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searches++
	if f.searches <= f.lag {
		return nil, nil
	}
	var out []plex.Track
	for _, t := range f.scan(true) {
		if strings.EqualFold(t.Title, title) && (artist == "" || strings.EqualFold(t.Artist, artist)) {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *placeFakePlex) Tracks(context.Context, string) ([]plex.Track, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scan(false), nil
}

func (f *placeFakePlex) Playlists(context.Context) ([]plex.Playlist, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []plex.Playlist
	for k, p := range f.playlists {
		out = append(out, plex.Playlist{RatingKey: k, Title: p.Title})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RatingKey < out[j].RatingKey })
	return out, nil
}

func (f *placeFakePlex) PlaylistItems(_ context.Context, key string) ([]plex.PlaylistItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.playlists[key]
	if !ok {
		return nil, fmt.Errorf("no playlist %s", key)
	}
	var out []plex.PlaylistItem
	for i, k := range p.Keys {
		out = append(out, plex.PlaylistItem{PlaylistItemID: fmt.Sprintf("%s-%d", key, i), RatingKey: k})
	}
	return out, nil
}

func (f *placeFakePlex) CreatePlaylist(_ context.Context, title string, keys []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextPL++
	key := fmt.Sprintf("pl%s%d", f.salt, f.nextPL)
	f.playlists[key] = &placeFakePlaylist{Title: title, Keys: append([]string(nil), keys...)}
	return key, nil
}

func (f *placeFakePlex) AddToPlaylist(_ context.Context, key string, keys []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dropAdds {
		return nil // accepted but not applied: only a read-back notices
	}
	p, ok := f.playlists[key]
	if !ok {
		return fmt.Errorf("no playlist %s", key)
	}
	p.Keys = append(p.Keys, keys...)
	return nil
}

func (f *placeFakePlex) RemoveFromPlaylist(_ context.Context, key, itemID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.playlists[key]
	if !ok {
		return fmt.Errorf("no playlist %s", key)
	}
	var idx int
	if _, err := fmt.Sscanf(strings.TrimPrefix(itemID, key+"-"), "%d", &idx); err != nil || idx >= len(p.Keys) {
		return fmt.Errorf("bad item id %s", itemID)
	}
	p.Keys = append(p.Keys[:idx:idx], p.Keys[idx+1:]...)
	return nil
}

func (f *placeFakePlex) SetRating(_ context.Context, key string, r float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ratings[key] = r
	return nil
}

// reimport simulates Plex re-importing the library: every rating key changes, ratings are
// lost, and every playlist survives by title but empty and under a new key. The mirror is
// gone entirely (a fresh server has none).
func (f *placeFakePlex) reimport(keepTitles ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.salt += "x"
	f.ratings = map[string]float64{}
	old := f.playlists
	f.playlists = map[string]*placeFakePlaylist{}
	for _, p := range old {
		for _, t := range keepTitles {
			if p.Title == t {
				f.nextPL++
				f.playlists[fmt.Sprintf("pl%s%d", f.salt, f.nextPL)] = &placeFakePlaylist{Title: p.Title}
			}
		}
	}
}

type placeFakeTagReader struct{}

func (placeFakeTagReader) ReadTags(_ context.Context, path string) (Tags, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Tags{}, err
	}
	_, t, err := placeDecode(b)
	if err != nil {
		return Tags{}, err
	}
	return Tags{Title: t.Title, Artist: t.Artist, Album: t.Album, RecordingMBID: t.RecordingMBID}, nil
}

// ---- rig ----

type placeRig struct {
	t      *testing.T
	v      *env
	lib    string
	plexRt string
	quar   string
	stage  string
	plex   *placeFakePlex
	chk    *placeFakeChecker
	ed     *placeFakeEditor
	euid   int
	chowns []string
	pl     *Placement
	measOK bool
}

func newPlaceRig(t *testing.T) *placeRig {
	t.Helper()
	root := t.TempDir()
	r := &placeRig{
		t: t, v: newEnv(t), lib: filepath.Join(root, "music"), quar: filepath.Join(root, "quarantine"),
		stage: filepath.Join(root, "staging"), plexRt: "/music", chk: &placeFakeChecker{}, ed: &placeFakeEditor{},
		euid: 0, measOK: true,
	}
	for _, d := range []string{r.lib, r.quar, r.stage} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r.plex = newPlaceFakePlex(r.lib, r.plexRt)
	r.pl = r.newPlacement()
	return r
}

func (r *placeRig) newPlacement() *Placement {
	r.t.Helper()
	pl, err := NewPlacement(PlacementConfig{
		LibraryRoot: r.lib, PlexRoot: r.plexRt, QuarantineRoot: r.quar,
		PlexSection: func() string { return "5" },
		Plex:        r.plex, Checker: r.chk, Tags: placeFakeTagReader{}, Editor: r.ed,
		Chown: func(path string, uid, gid int) error {
			r.chowns = append(r.chowns, fmt.Sprintf("%s %d:%d", filepath.Base(path), uid, gid))
			return nil
		},
		Euid:    func() int { return r.euid },
		Measure: func(string) bool { return r.measOK },
		Sleep:   func(context.Context, time.Duration) {},
	})
	if err != nil {
		r.t.Fatal(err)
	}
	pl.Bind(r.v.e)
	return pl
}

// restart is a process restart: a fresh Placement over the same database and disk.
func (r *placeRig) restart() *Placement {
	r.pl = r.newPlacement()
	return r.pl
}

type placeSeed struct {
	MBID, Title, Artist, Album string
	Disc, Track                int
	Audio                      string
	AlbumArtist                string
	// Others are the rest of the release, written beside the track in staging.
	Others []placeOther
}

type placeOther struct {
	Title       string
	Disc, Track int
	Audio       string
	Artist      string
	MBID        string
}

type placeSeeded struct {
	ItemID, CandID, RecID int64
	Src                   string
	SHA, FP               string
}

func placeWrite(t *testing.T, path string, audio string, tags TrackTags) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	b := placeEncode(audio, tags)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// seed creates a recording, an item in placing, and a chosen candidate whose staged file
// has the checksum and fingerprint the acquirer would have recorded.
func (r *placeRig) seed(s placeSeed) placeSeeded {
	r.t.Helper()
	if s.MBID == "" {
		s.MBID = "mbid-" + s.Title
	}
	if s.Audio == "" {
		s.Audio = "AUDIO-" + s.Title
	}
	rec := r.v.seedRecording(s.MBID, s.Title, s.Artist, "", "studio")
	item := r.v.seedItem(KindRequest, StatePlacing, rec)
	res, err := r.v.db.Exec(
		`INSERT INTO mus_candidates (item_id, lane, state, release_name, release_mbid, created_at) VALUES (?, 'usenet', 'chosen', 'rel', 'rel-mbid', ?)`,
		item, r.v.clock.Now().Unix())
	if err != nil {
		r.t.Fatal(err)
	}
	cand, _ := res.LastInsertId()
	dir := filepath.Join(r.stage, fmt.Sprint(item), fmt.Sprint(cand))
	src := filepath.Join(dir, fmt.Sprintf("%02d-%s.flac", s.Track, s.Title))
	sha := placeWrite(r.t, src, s.Audio, TrackTags{Artist: s.Artist, AlbumArtist: s.AlbumArtist, Album: s.Album, Title: s.Title, Disc: s.Disc, Track: s.Track, RecordingMBID: s.MBID})
	for _, o := range s.Others {
		art := o.Artist
		if art == "" {
			art = s.Artist
		}
		placeWrite(r.t, filepath.Join(dir, fmt.Sprintf("d%d-%02d-%s.flac", o.Disc, o.Track, o.Title)), firstNonEmpty(o.Audio, "AUDIO-"+o.Title),
			TrackTags{Artist: art, AlbumArtist: s.AlbumArtist, Album: s.Album, Title: o.Title, Disc: o.Disc, Track: o.Track, RecordingMBID: o.MBID})
	}
	fp := placeFP(s.Audio)
	_, err = r.v.db.Exec(`UPDATE mus_candidates SET staging_path = ?, sha256 = ?, pcm_fingerprint = ?, codec = 'flac', container = 'flac',
		bit_depth = 16, sample_rate = 44100, channels = 2, verdict = 'GENUINE' WHERE id = ?`, src, sha, fp, cand)
	if err != nil {
		r.t.Fatal(err)
	}
	return placeSeeded{ItemID: item, CandID: cand, RecID: rec, Src: src, SHA: sha, FP: fp}
}

func (r *placeRig) step(id int64) error {
	r.t.Helper()
	return r.pl.Step(context.Background(), r.v.e, r.v.item(id))
}

// drive steps the item until it is done (or fails the test after n steps).
func (r *placeRig) drive(id int64, n int) {
	r.t.Helper()
	for i := 0; i < n; i++ {
		it := r.v.item(id)
		if it.State == StateDone {
			return
		}
		if err := r.pl.Step(context.Background(), r.v.e, it); err != nil {
			r.t.Fatalf("step %d in %s: %v", i, it.State, err)
		}
	}
	if it := r.v.item(id); it.State != StateDone {
		r.t.Fatalf("item %d is %s after %d steps (reason %q)", id, it.State, n, it.LastReason)
	}
}

// files lists every regular file below root as rel path -> "sha mode".
func placeTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		fi, _ := d.Info()
		sha, _ := placeSHA256(p)
		rel, _ := filepath.Rel(root, p)
		out[rel] = fmt.Sprintf("%s %o", sha[:12], fi.Mode().Perm())
		return nil
	})
	return out
}

func placeTemps(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && journalIsTemp(d.Name()) {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func (r *placeRig) libFlacs() []string {
	var out []string
	_ = filepath.WalkDir(r.lib, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !journalIsTemp(d.Name()) && strings.HasSuffix(p, ".flac") {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// ---- tests ----

func TestPlaceNewTrackLandsInExistingArtistFolderWithOwnerAndMode(t *testing.T) {
	r := newPlaceRig(t)
	// The library already has "The Black Keys" with an album folder named with " - " style.
	existing := filepath.Join(r.lib, "The Black Keys", "El Camino (2011)")
	placeWrite(t, filepath.Join(existing, "01 - Lonely Boy.flac"), "OLD", TrackTags{Artist: "The Black Keys", Album: "El Camino", Title: "Lonely Boy", Track: 1})
	s := r.seed(placeSeed{Title: "Gold on the Ceiling", Artist: "black keys", AlbumArtist: "Black Keys", Album: "El Camino", Disc: 1, Track: 2})

	r.drive(s.ItemID, 4)

	want := filepath.Join(existing, "02 - Gold on the Ceiling.flac")
	if got := r.libFlacs(); len(got) != 2 {
		t.Fatalf("library flacs = %v, want the old track and the new one only", got)
	}
	fi, err := os.Stat(want)
	if err != nil {
		t.Fatalf("expected %s: %v", want, err)
	}
	if fi.Mode().Perm() != 0o664 {
		t.Errorf("mode = %o, want 664", fi.Mode().Perm())
	}
	// Owner is set on the temp name; the published name is a hard link to the same file.
	if len(r.chowns) != 1 || !strings.HasSuffix(r.chowns[0], "02 - Gold on the Ceiling.flac 1024:100") {
		t.Errorf("chown calls = %v, want 1024:100 once on the new file", r.chowns)
	}
	if _, err := os.Stat(filepath.Join(r.lib, "Black Keys")); err == nil {
		t.Fatal("a second artist folder was created")
	}
	var published, tempSHA string
	var phase int
	if err := r.v.db.QueryRow(`SELECT published_sha256, temp_sha256, phase FROM mus_swap_journal WHERE item_id = ?`, s.ItemID).Scan(&published, &tempSHA, &phase); err != nil {
		t.Fatal(err)
	}
	onDisk, _ := placeSHA256(want)
	if published != onDisk || tempSHA != onDisk || phase != PhasePlexConfirmed {
		t.Errorf("published %s temp %s disk %s phase %d: want all checksums equal and phase 5", published, tempSHA, onDisk, phase)
	}
	if left := placeTemps(t, r.lib); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
	if it := r.v.item(s.ItemID); it.State != StateDone || it.TrackID == 0 {
		t.Errorf("item = %s track %d, want done with a library track", it.State, it.TrackID)
	}
	// R16: facts recorded on the library track.
	var codec string
	var depth int
	if err := r.v.db.QueryRow(`SELECT codec, bit_depth FROM mus_library_tracks WHERE id = ?`, r.v.item(s.ItemID).TrackID).Scan(&codec, &depth); err != nil || codec != "flac" || depth != 16 {
		t.Errorf("library track facts = %q %d (%v), want flac 16", codec, depth, err)
	}
	// Plex was asked about the Plex-side path, not ours.
	if len(r.plex.refreshes) == 0 || !strings.HasPrefix(r.plex.refreshes[0], "/music/The Black Keys/El Camino (2011)") {
		t.Errorf("refreshes = %v, want the mapped Plex path", r.plex.refreshes)
	}
}

func TestPlaceNewFolderUsesNumberedNameAndContinuousDiscs(t *testing.T) {
	r := newPlaceRig(t)
	var others []placeOther
	for i := 1; i <= 12; i++ { // disc 1 has 12 tracks
		others = append(others, placeOther{Title: fmt.Sprintf("D1T%d", i), Disc: 1, Track: i})
	}
	s := r.seed(placeSeed{Title: "Encore", Artist: "Band", Album: "Live!", Disc: 2, Track: 1, Others: others})
	r.drive(s.ItemID, 4)

	want := filepath.Join(r.lib, "Band", "Live!", "13. Encore.flac")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected %s: %v (have %v)", want, err, r.libFlacs())
	}
	if len(r.ed.writes) != 1 || r.ed.writes[0] != fmt.Sprintf(".rmc-tmp-%d-13. Encore.flac d2 t13", 1) {
		t.Errorf("tag writes = %v, want disc 2 track 13 written to the temp file", r.ed.writes)
	}
	tags, _ := r.ed.Read(context.Background(), want)
	if tags.Disc != 2 || tags.Track != 13 {
		t.Errorf("tags on the placed file = disc %d track %d, want 2/13", tags.Disc, tags.Track)
	}
}

func TestPlaceSkipsChownWhenNotRootAndRecordsWhy(t *testing.T) {
	r := newPlaceRig(t)
	r.euid = 1000
	s := r.seed(placeSeed{Title: "Song", Artist: "Band", Album: "Album", Disc: 1, Track: 1})
	r.drive(s.ItemID, 4)
	if len(r.chowns) != 0 {
		t.Errorf("chown ran as non-root: %v", r.chowns)
	}
	var note string
	if err := r.v.db.QueryRow(`SELECT last_error FROM mus_swap_journal WHERE item_id = ?`, s.ItemID).Scan(&note); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "chown 1024:100 skipped: not running as root") {
		t.Errorf("journal note = %q, want the chown-skipped reason", note)
	}
}

func TestPlaceIntegrityFailureRollsBackAndLeavesNothingInTheLibrary(t *testing.T) {
	r := newPlaceRig(t)
	s := r.seed(placeSeed{Title: "Bad", Artist: "Band", Album: "Album", Disc: 1, Track: 1, Audio: "CORRUPT-AUDIO"})
	// the acquirer's own fingerprint would match; the in-place decode is what fails
	if err := r.step(s.ItemID); err != nil {
		t.Fatalf("step: %v", err)
	}
	if got := r.libFlacs(); len(got) != 0 {
		t.Errorf("library has %v after an integrity failure", got)
	}
	if left := placeTemps(t, r.lib); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
	var closed int64
	var why string
	if err := r.v.db.QueryRow(`SELECT closed_at, last_error FROM mus_swap_journal WHERE item_id = ?`, s.ItemID).Scan(&closed, &why); err != nil {
		t.Fatal(err)
	}
	if closed == 0 || !strings.Contains(why, "rolled back") {
		t.Errorf("journal row closed_at %d, last_error %q: want closed as rolled back", closed, why)
	}
	if it := r.v.item(s.ItemID); it.State != StateSearching {
		t.Errorf("item = %s, want searching (next candidate)", it.State)
	}
	var cst string
	_ = r.v.db.QueryRow(`SELECT state FROM mus_candidates WHERE id = ?`, s.CandID).Scan(&cst)
	if cst != "rejected" {
		t.Errorf("candidate = %s, want rejected so it is not chosen again", cst)
	}
}

func TestPlaceAudioChangedByTaggingIsRolledBack(t *testing.T) {
	r := newPlaceRig(t)
	s := r.seed(placeSeed{Title: "Song", Artist: "Band", Album: "Album", Disc: 1, Track: 1})
	// The candidate was fingerprinted from different audio than the staged file holds now.
	if _, err := r.v.db.Exec(`UPDATE mus_candidates SET pcm_fingerprint = 'someone-elses' WHERE id = ?`, s.CandID); err != nil {
		t.Fatal(err)
	}
	if err := r.step(s.ItemID); err != nil {
		t.Fatalf("step: %v", err)
	}
	if got := r.libFlacs(); len(got) != 0 {
		t.Errorf("library has %v despite a fingerprint mismatch", got)
	}
	if it := r.v.item(s.ItemID); it.State != StateSearching {
		t.Errorf("item = %s, want searching", it.State)
	}
}

func TestPlaceBlindCheckIsRetriedNotRolledIntoARejection(t *testing.T) {
	r := newPlaceRig(t)
	s := r.seed(placeSeed{Title: "Song", Artist: "Band", Album: "Album", Disc: 1, Track: 1, Audio: "BLIND-AUDIO"})
	err := r.step(s.ItemID)
	if err == nil || !strings.Contains(err.Error(), "blind") {
		t.Fatalf("step err = %v, want a blind-check error", err)
	}
	if it := r.v.item(s.ItemID); it.State != StatePlacing {
		t.Errorf("item = %s, want placing (retry)", it.State)
	}
	var cst string
	_ = r.v.db.QueryRow(`SELECT state FROM mus_candidates WHERE id = ?`, s.CandID).Scan(&cst)
	if cst != "chosen" {
		t.Errorf("candidate = %s, want still chosen: blind is not evidence of bad audio", cst)
	}
	// The share comes back readable: the next step settles the stale row and places the file.
	if err := os.WriteFile(s.Src, placeEncode("GOOD-AUDIO", TrackTags{Artist: "Band", Album: "Album", Title: "Song", Disc: 1, Track: 1}), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, _ := placeSHA256(s.Src)
	if _, err := r.v.db.Exec(`UPDATE mus_candidates SET sha256 = ?, pcm_fingerprint = ? WHERE id = ?`, sum, placeFP("GOOD-AUDIO"), s.CandID); err != nil {
		t.Fatal(err)
	}
	r.drive(s.ItemID, 5)
	if got := r.libFlacs(); len(got) != 1 {
		t.Errorf("library = %v, want exactly one file", got)
	}
}

func TestPlaceCollisionLeavesUnrelatedFileUntouchedAndRaisesDecision(t *testing.T) {
	r := newPlaceRig(t)
	dest := filepath.Join(r.lib, "Band", "Album", "01. Song.flac")
	placeWrite(t, dest, "SOMEONE-ELSES-FILE", TrackTags{Artist: "Band", Album: "Album", Title: "Other", Track: 1})
	before, _ := placeSHA256(dest)
	s := r.seed(placeSeed{Title: "Song", Artist: "Band", Album: "Album", Disc: 1, Track: 1})

	if err := r.step(s.ItemID); err != nil {
		t.Fatalf("step: %v", err)
	}
	after, _ := placeSHA256(dest)
	if before != after {
		t.Fatal("the unrelated file was changed")
	}
	if left := placeTemps(t, r.lib); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
	if n := r.v.count(`SELECT COUNT(*) FROM mus_decisions WHERE item_id = ? AND kind = 'collision' AND answered_at = 0`, s.ItemID); n != 1 {
		t.Errorf("open collision decisions = %d, want 1", n)
	}
	if it := r.v.item(s.ItemID); it.State != StateWaitingBlair {
		t.Errorf("item = %s, want waiting_blair", it.State)
	}
}

func TestPlaceItemStaysAwaitingUntilPlexIndexesThenDone(t *testing.T) {
	r := newPlaceRig(t)
	r.plex.lag = 5 // Plex lists the file only after five searches
	s := r.seed(placeSeed{Title: "Song", Artist: "Band", Album: "Album", Disc: 1, Track: 1})

	if err := r.step(s.ItemID); err != nil { // placing -> published -> awaiting
		t.Fatal(err)
	}
	if it := r.v.item(s.ItemID); it.State != StateAwaitingPlexIndex {
		t.Fatalf("after placing: %s, want awaiting_plex_index", it.State)
	}
	if err := r.step(s.ItemID); err != nil { // 3 polls, none list it
		t.Fatal(err)
	}
	it := r.v.item(s.ItemID)
	if it.State != StateAwaitingPlexIndex || it.NextAttemptAt == 0 {
		t.Fatalf("after first poll round: %s next %d, want awaiting with a retry time", it.State, it.NextAttemptAt)
	}
	if n := r.v.count(`SELECT COUNT(*) FROM mus_swap_journal WHERE item_id = ? AND phase = 4 AND closed_at = 0`, s.ItemID); n != 1 {
		t.Errorf("journal should sit at published, open; rows = %d", n)
	}
	if err := r.step(s.ItemID); err != nil { // searches 4,5 empty, 6 lists it
		t.Fatal(err)
	}
	if it := r.v.item(s.ItemID); it.State != StateDone {
		t.Errorf("after Plex listed it: %s, want done", it.State)
	}
	var key string
	_ = r.v.db.QueryRow(`SELECT plex_rating_key FROM mus_library_tracks WHERE id = ?`, r.v.item(s.ItemID).TrackID).Scan(&key)
	if key == "" {
		t.Error("library track has no Plex rating key")
	}
}

func TestPlaceRefusesWhenLibraryCannotBeMeasuredOrIsMissing(t *testing.T) {
	r := newPlaceRig(t)
	s := r.seed(placeSeed{Title: "Song", Artist: "Band", Album: "Album", Disc: 1, Track: 1})

	r.measOK = false
	err := r.step(s.ItemID)
	if !errors.Is(err, ErrLibraryUnreachable) {
		t.Fatalf("unmeasurable: err = %v, want ErrLibraryUnreachable", err)
	}
	if it := r.v.item(s.ItemID); it.State != StatePlacing {
		t.Errorf("item = %s, want placing", it.State)
	}
	if entries, _ := os.ReadDir(r.lib); len(entries) != 0 {
		t.Errorf("library root was written to: %v", entries)
	}

	// A missing root (an unmounted share) is never created.
	r.measOK = true
	if err := os.Remove(r.lib); err != nil {
		t.Fatal(err)
	}
	err = r.step(s.ItemID)
	if !errors.Is(err, ErrLibraryUnreachable) {
		t.Fatalf("missing root: err = %v, want ErrLibraryUnreachable", err)
	}
	if _, err := os.Stat(r.lib); err == nil {
		t.Fatal("the library root was recreated")
	}
	if n := r.v.count(`SELECT COUNT(*) FROM mus_swap_journal`); n != 0 {
		t.Errorf("journal rows = %d, want none: nothing was attempted", n)
	}
}

func TestPlaceNonFlacKeepsTagsAndSaysWhy(t *testing.T) {
	r := newPlaceRig(t)
	rec := r.v.seedRecording("mb-yt", "Song", "Band", "", "studio")
	item := r.v.seedItem(KindRequest, StatePlacing, rec)
	res, _ := r.v.db.Exec(`INSERT INTO mus_candidates (item_id, lane, state, created_at) VALUES (?, 'youtube', 'chosen', ?)`, item, r.v.clock.Now().Unix())
	cand, _ := res.LastInsertId()
	src := filepath.Join(r.stage, "yt", "song.m4a")
	sha := placeWrite(t, src, "LOSSY-AUDIO", TrackTags{Artist: "Band", Album: "Song", Title: "Song"})
	if _, err := r.v.db.Exec(`UPDATE mus_candidates SET staging_path = ?, sha256 = ?, pcm_fingerprint = ?, codec = 'aac', verdict = 'LOSSY' WHERE id = ?`,
		src, sha, placeFP("LOSSY-AUDIO"), cand); err != nil {
		t.Fatal(err)
	}
	r.drive(item, 4)
	want := filepath.Join(r.lib, "Band", "Song", "01. Song.m4a")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected %s: %v", want, err)
	}
	if len(r.ed.writes) != 0 {
		t.Errorf("metaflac ran on a non-FLAC file: %v", r.ed.writes)
	}
	var note string
	_ = r.v.db.QueryRow(`SELECT last_error FROM mus_swap_journal WHERE item_id = ?`, item).Scan(&note)
	if !strings.Contains(note, "not FLAC") {
		t.Errorf("journal note = %q, want why tag numbering was left alone", note)
	}
}

func TestPlaceStepDrivenByEngineTick(t *testing.T) {
	r := newPlaceRig(t)
	e, err := New(Config{DB: r.v.db, Placer: r.pl, Acquirer: r.v.acquirer, Notifier: r.v.notifier, Now: r.v.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	s := r.seed(placeSeed{Title: "Song", Artist: "Band", Album: "Album", Disc: 1, Track: 1})
	for i := 0; i < 4; i++ {
		if err := e.Tick(context.Background()); err != nil {
			t.Logf("tick: %v", err)
		}
	}
	if it := r.v.item(s.ItemID); it.State != StateDone {
		t.Errorf("after ticks: %s (%s), want done", it.State, it.LastReason)
	}
}

func TestPlaceSwapStatesAreLeftToU11(t *testing.T) {
	r := newPlaceRig(t)
	id := r.v.seedItem(KindUpgrade, StateSwapping, 0)
	if err := r.step(id); err != nil {
		t.Fatalf("swapping with no swap handler: %v, want a quiet no-op", err)
	}
	if it := r.v.item(id); it.State != StateSwapping {
		t.Errorf("item moved to %s", it.State)
	}
	var seen []State
	r.pl.cfg.Swaps = placerFunc(func(_ context.Context, _ Machine, it Item) error { seen = append(seen, it.State); return nil })
	if err := r.step(id); err != nil || len(seen) != 1 || seen[0] != StateSwapping {
		t.Errorf("swap handler calls = %v err %v, want one swapping call", seen, err)
	}
}

type placerFunc func(ctx context.Context, m Machine, it Item) error

func (f placerFunc) Step(ctx context.Context, m Machine, it Item) error { return f(ctx, m, it) }
