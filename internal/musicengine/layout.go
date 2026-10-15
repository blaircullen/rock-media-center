package musicengine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/tristenlammi/arrmada/internal/diskspace"
)

// This file is placement's view of the library on disk (R17, KTD7): find an artist's existing
// folder by a normalized name and reuse its naming, plan `Artist/Album/NN. Title.ext` for a
// new album, and fence every write inside the library root. It never creates the root itself:
// if the share is not mounted, MkdirAll would quietly write to local disk under the mount point.

// ErrLibraryUnreachable means the library root is missing or cannot be measured. The item
// stays where it is and nothing is written (R28).
var ErrLibraryUnreachable = errors.New("musicengine: library root is unreachable")

// ErrOutsideLibrary means a path resolves outside the library root.
var ErrOutsideLibrary = errors.New("musicengine: path is outside the library root")

// Layout is the library tree below Root.
type Layout struct {
	// Root is the absolute library root, e.g. /mnt/nas/music.
	Root string
	// Measure reports whether the filesystem holding a path can be measured. Nil means
	// diskspace.Of, which cannot measure off Linux; tests inject their own.
	Measure func(path string) bool
}

func (l *Layout) measure(path string) bool {
	if l.Measure != nil {
		return l.Measure(path)
	}
	_, ok := diskspace.Of(path)
	return ok
}

// Check refuses to proceed unless the root is an existing directory on a measurable
// filesystem. It never creates anything.
func (l *Layout) Check() error {
	if strings.TrimSpace(l.Root) == "" || !filepath.IsAbs(l.Root) {
		return fmt.Errorf("%w: root %q is not an absolute path", ErrLibraryUnreachable, l.Root)
	}
	fi, err := os.Stat(l.Root)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrLibraryUnreachable, l.Root, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrLibraryUnreachable, l.Root)
	}
	if !l.measure(l.Root) {
		return fmt.Errorf("%w: space on %s cannot be measured, so the share is treated as unreachable", ErrLibraryUnreachable, l.Root)
	}
	return nil
}

// layoutDeepestExisting returns the longest existing ancestor of path (path itself if it exists).
func layoutDeepestExisting(path string) string {
	p := filepath.Clean(path)
	for {
		if _, err := os.Lstat(p); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

// Inside refuses any path that is not below the root, lexically or once symlinks on its
// deepest existing parent are resolved (a symlinked artist folder cannot escape the root).
func (l *Layout) Inside(path string) error {
	return layoutInside(l.Root, path)
}

func layoutInside(root, path string) error {
	root, path = filepath.Clean(root), filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: %s is not below %s", ErrOutsideLibrary, path, root)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("%w: resolving %s: %v", ErrLibraryUnreachable, root, err)
	}
	anc := layoutDeepestExisting(path)
	realAnc, err := filepath.EvalSymlinks(anc)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", anc, err)
	}
	r2, err := filepath.Rel(realRoot, realAnc)
	if err != nil || r2 == ".." || strings.HasPrefix(r2, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: %s resolves to %s, outside %s", ErrOutsideLibrary, path, realAnc, realRoot)
	}
	return nil
}

// layoutNorm folds a folder name for matching: case, punctuation, spacing and a leading
// "The" do not matter ("The Black Keys", "black keys" and "Black-Keys!" are one artist).
func layoutNorm(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "&", " and ")
	s = strings.TrimPrefix(s, "the ")
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

var layoutYearSuffix = regexp.MustCompile(`\s*[\(\[]\d{4}[\)\]]\s*$`)

// layoutNormAlbum is layoutNorm that also ignores a trailing "(2010)" year, because some
// libraries file "Album (2010)" and tags say "Album".
func layoutNormAlbum(s string) string {
	return layoutNorm(layoutYearSuffix.ReplaceAllString(strings.TrimSpace(s), ""))
}

// layoutSanitize makes a name safe as one path element: the characters the getmusic skill
// strips become "_", and leading/trailing dots and spaces go.
func layoutSanitize(s string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_")
	s = r.Replace(strings.TrimSpace(s))
	s = strings.Trim(s, ". ")
	return s
}

// layoutFindDir finds the sub-directory of parent whose normalized name matches. When more
// than one matches it prefers an exact (case-sensitive) name, then the first sorted, so the
// answer is deterministic.
func layoutFindDir(parent, want string, norm func(string) string) (string, bool, error) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("reading %s: %w", parent, err)
	}
	target := norm(want)
	if target == "" {
		return "", false, nil
	}
	var matches []string
	for _, e := range entries {
		if !e.IsDir() && e.Type()&os.ModeSymlink == 0 {
			continue
		}
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if norm(e.Name()) == target {
			matches = append(matches, e.Name())
		}
	}
	if len(matches) == 0 {
		return "", false, nil
	}
	sort.Strings(matches)
	for _, m := range matches {
		if m == want {
			return filepath.Join(parent, m), true, nil
		}
	}
	return filepath.Join(parent, matches[0]), true, nil
}

// FindArtistDir finds an existing artist folder by normalized name.
func (l *Layout) FindArtistDir(artist string) (string, bool, error) {
	return layoutFindDir(l.Root, artist, layoutNorm)
}

// FindAlbumDir finds an existing album folder under an artist folder.
func (l *Layout) FindAlbumDir(artistDir, album string) (string, bool, error) {
	return layoutFindDir(artistDir, album, layoutNormAlbum)
}

var layoutNumPrefix = regexp.MustCompile(`^\d+(?:-\d+)?( - |\. | )`)

// layoutTrackSeparator reads how the files already in an album folder join the number to
// the title (". ", " - " or " ") and returns the most common; the default is ". ".
func layoutTrackSeparator(albumDir string) string {
	entries, err := os.ReadDir(albumDir)
	if err != nil {
		return ". "
	}
	counts := map[string]int{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if m := layoutNumPrefix.FindStringSubmatch(e.Name()); m != nil {
			counts[m[1]]++
		}
	}
	best, n := ". ", 0
	for _, sep := range []string{". ", " - ", " "} { // fixed order: ties go to the plan's own style
		if counts[sep] > n {
			best, n = sep, counts[sep]
		}
	}
	return best
}

// Planned is where one track will live.
type Planned struct {
	ArtistDir, AlbumDir, Path string
	NewArtist, NewAlbum       bool
}

// Plan decides the final path of one track. An existing artist folder (and album folder
// within it) is reused with its own spelling; otherwise new ones are named from the tags.
// number is the continuous track number (layoutNumberFor). Nothing is created.
func (l *Layout) Plan(artist, album string, number int, title, ext string) (Planned, error) {
	if err := l.Check(); err != nil {
		return Planned{}, err
	}
	artist, album, title = strings.TrimSpace(artist), strings.TrimSpace(album), strings.TrimSpace(title)
	if layoutSanitize(artist) == "" {
		return Planned{}, errors.New("musicengine layout: no artist to file the track under")
	}
	if layoutSanitize(title) == "" {
		return Planned{}, errors.New("musicengine layout: no title to name the track")
	}
	if album == "" {
		album = title // a single with no album tag is filed under its own title
	}
	var pl Planned
	dir, found, err := l.FindArtistDir(artist)
	if err != nil {
		return Planned{}, err
	}
	if !found {
		dir, pl.NewArtist = filepath.Join(l.Root, layoutSanitize(artist)), true
	}
	pl.ArtistDir = dir
	adir := ""
	if !pl.NewArtist {
		if adir, found, err = l.FindAlbumDir(dir, album); err != nil {
			return Planned{}, err
		}
	}
	if adir == "" {
		if layoutSanitize(album) == "" {
			return Planned{}, errors.New("musicengine layout: album name is empty after cleaning")
		}
		adir, pl.NewAlbum = filepath.Join(dir, layoutSanitize(album)), true
	}
	pl.AlbumDir = adir
	sep := ". "
	if !pl.NewAlbum {
		sep = layoutTrackSeparator(adir)
	}
	if number < 1 {
		number = 1
	}
	name := fmt.Sprintf("%02d%s%s%s", number, sep, layoutSanitize(title), strings.ToLower(ext))
	pl.Path = filepath.Join(adir, name)
	if err := l.Inside(pl.Path); err != nil {
		return Planned{}, err
	}
	return pl, nil
}

// MkdirInside creates the missing folders from the root down to dir, one level at a time,
// never the root itself. fix runs on each created folder (mode and owner).
func (l *Layout) MkdirInside(dir string, perm os.FileMode, fix func(path string) error) error {
	if err := l.Check(); err != nil {
		return err
	}
	if err := l.Inside(filepath.Join(dir, "x")); err != nil { // dir itself may equal an existing folder
		return err
	}
	rel, err := filepath.Rel(filepath.Clean(l.Root), filepath.Clean(dir))
	if err != nil {
		return err
	}
	cur := filepath.Clean(l.Root)
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		if fi, err := os.Stat(cur); err == nil {
			if !fi.IsDir() {
				return fmt.Errorf("%s exists and is not a directory", cur)
			}
			continue
		}
		if err := l.Inside(cur); err != nil {
			return err
		}
		if err := os.Mkdir(cur, perm); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("creating %s: %w", cur, err)
		}
		if err := os.Chmod(cur, perm); err != nil { // the umask trims the mode Mkdir was given
			return fmt.Errorf("setting mode on %s: %w", cur, err)
		}
		if fix != nil {
			if err := fix(cur); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- numbering ----

// layoutTrack is a track's tagged position: disc 0 counts as disc 1.
type layoutTrack struct {
	Key         string // identifies the file (its staged path)
	Disc, Track int
}

// layoutNumber is a track's final numbering: the tags carry Disc and Continuous, the file
// name carries Continuous (disc 2 track 1 of a 12-track disc 1 is 13, as getmusic does).
type layoutNumber struct {
	Key        string
	Disc       int
	DiscTrack  int
	Continuous int
}

// layoutNumberAlbum numbers every track of an album. When the tagged positions are not sane
// (a track number below 1, or two tracks claiming one disc and track, as a WEB-FLAC once
// tagged its encore disc 1/1-4) the whole set is renumbered by sorted file key on one disc
// and repaired is true.
func layoutNumberAlbum(in []layoutTrack) (out []layoutNumber, repaired bool) {
	seen := map[[2]int]bool{}
	sane := true
	for _, t := range in {
		d := max(t.Disc, 1)
		if t.Track < 1 || seen[[2]int{d, t.Track}] {
			sane = false
			break
		}
		seen[[2]int{d, t.Track}] = true
	}
	if !sane {
		keys := append([]layoutTrack(nil), in...)
		sort.Slice(keys, func(i, j int) bool { return keys[i].Key < keys[j].Key })
		for i, t := range keys {
			out = append(out, layoutNumber{Key: t.Key, Disc: 1, DiscTrack: i + 1, Continuous: i + 1})
		}
		return out, true
	}
	maxOnDisc := map[int]int{}
	for _, t := range in {
		d := max(t.Disc, 1)
		maxOnDisc[d] = max(maxOnDisc[d], t.Track)
	}
	for _, t := range in {
		d := max(t.Disc, 1)
		prior := 0
		for od, m := range maxOnDisc {
			if od < d {
				prior += m
			}
		}
		out = append(out, layoutNumber{Key: t.Key, Disc: d, DiscTrack: t.Track, Continuous: prior + t.Track})
	}
	return out, false
}

// layoutNumberFor numbers one track given the other tracks known to be on its release (the
// rest of the release in staging). With no usable tags it is disc 1 track 1.
func layoutNumberFor(self layoutTrack, siblings []layoutTrack) (layoutNumber, bool) {
	if self.Track < 1 {
		return layoutNumber{Key: self.Key, Disc: max(self.Disc, 1), DiscTrack: 1, Continuous: 1}, true
	}
	all := []layoutTrack{self}
	for _, s := range siblings {
		if s.Key != self.Key {
			all = append(all, s)
		}
	}
	nums, repaired := layoutNumberAlbum(all)
	if repaired {
		// Siblings with clashing tags must not renumber the track we are placing; use
		// only its own tagged position.
		own, _ := layoutNumberAlbum([]layoutTrack{self})
		return own[0], true
	}
	for _, n := range nums {
		if n.Key == self.Key {
			return n, false
		}
	}
	return layoutNumber{Key: self.Key, Disc: max(self.Disc, 1), DiscTrack: self.Track, Continuous: self.Track}, false
}
