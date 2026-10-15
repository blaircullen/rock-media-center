package musicengine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mkLib(t *testing.T) *Layout {
	t.Helper()
	root := filepath.Join(t.TempDir(), "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return &Layout{Root: root, Measure: func(string) bool { return true }}
}

func TestLayoutFindsArtistFolderIgnoringCasePunctuationAndThe(t *testing.T) {
	l := mkLib(t)
	for _, d := range []string{"The Black Keys", "AC_DC", "Guns N' Roses"} {
		if err := os.MkdirAll(filepath.Join(l.Root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]string{
		"Black Keys": "The Black Keys", "the black keys": "The Black Keys", "AC/DC": "AC_DC", "ac dc": "AC_DC",
		"Guns N Roses": "Guns N' Roses", "Guns n' Roses!": "Guns N' Roses",
	}
	for in, want := range cases {
		got, ok, err := l.FindArtistDir(in)
		if err != nil || !ok || filepath.Base(got) != want {
			t.Errorf("FindArtistDir(%q) = %q %v %v, want %q", in, got, ok, err, want)
		}
	}
	if _, ok, _ := l.FindArtistDir("Someone Else"); ok {
		t.Error("found a folder that does not exist")
	}
}

func TestLayoutPlanNeverMakesASecondFolderForAnExistingArtist(t *testing.T) {
	l := mkLib(t)
	existing := filepath.Join(l.Root, "The Black Keys", "El Camino (2011)")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"01 - Lonely Boy.flac", "02 - Gold.flac"} {
		if err := os.WriteFile(filepath.Join(existing, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pl, err := l.Plan("Black Keys", "El Camino", 3, "Dead and Gone", ".FLAC")
	if err != nil {
		t.Fatal(err)
	}
	if pl.NewArtist || pl.NewAlbum || pl.Path != filepath.Join(existing, "03 - Dead and Gone.flac") {
		t.Errorf("plan = %+v, want the existing folders and their ' - ' naming", pl)
	}
	// An existing artist with a new album gets a new album folder inside it.
	pl, err = l.Plan("The Black Keys", "Brothers", 1, "Everlasting Light", ".flac")
	if err != nil || pl.NewArtist || !pl.NewAlbum || pl.Path != filepath.Join(l.Root, "The Black Keys", "Brothers", "01. Everlasting Light.flac") {
		t.Errorf("new album plan = %+v %v", pl, err)
	}
	// A new artist is named from the tags, cleaned.
	pl, err = l.Plan("New: Band?", "Debut/EP", 12, "Hey: You", ".flac")
	if err != nil || !pl.NewArtist || pl.Path != filepath.Join(l.Root, "New_ Band_", "Debut_EP", "12. Hey_ You.flac") {
		t.Errorf("new artist plan = %+v %v", pl, err)
	}
}

func TestLayoutRefusesUnreachableRootAndNeverCreatesIt(t *testing.T) {
	root := filepath.Join(t.TempDir(), "unmounted")
	l := &Layout{Root: root, Measure: func(string) bool { return true }}
	if err := l.Check(); !errors.Is(err, ErrLibraryUnreachable) {
		t.Errorf("missing root: %v", err)
	}
	if _, err := l.Plan("A", "B", 1, "C", ".flac"); !errors.Is(err, ErrLibraryUnreachable) {
		t.Errorf("Plan on a missing root: %v", err)
	}
	if err := l.MkdirInside(filepath.Join(root, "Artist", "Album"), 0o775, nil); !errors.Is(err, ErrLibraryUnreachable) {
		t.Errorf("MkdirInside on a missing root: %v", err)
	}
	if _, err := os.Stat(root); err == nil {
		t.Fatal("the missing root was created")
	}
	// A root that exists but cannot be measured is treated as unreachable too.
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	l.Measure = func(string) bool { return false }
	if err := l.Check(); !errors.Is(err, ErrLibraryUnreachable) || !strings.Contains(err.Error(), "cannot be measured") {
		t.Errorf("unmeasurable root: %v", err)
	}
	if err := (&Layout{Root: "relative/path"}).Check(); !errors.Is(err, ErrLibraryUnreachable) {
		t.Errorf("relative root: %v", err)
	}
}

func TestLayoutFenceRefusesPathsOutsideTheRootIncludingThroughSymlinks(t *testing.T) {
	l := mkLib(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(l.Root, "Escape")); err != nil {
		t.Fatal(err)
	}
	bad := []string{
		filepath.Join(l.Root, "..", "evil.flac"),
		filepath.Join(l.Root, "A", "..", "..", "evil.flac"),
		l.Root, // the root itself is not a file location
		filepath.Join(outside, "x.flac"),
		filepath.Join(l.Root, "Escape", "Album", "x.flac"), // symlinked artist folder
	}
	for _, p := range bad {
		if err := l.Inside(p); !errors.Is(err, ErrOutsideLibrary) {
			t.Errorf("Inside(%s) = %v, want ErrOutsideLibrary", p, err)
		}
	}
	if err := l.Inside(filepath.Join(l.Root, "Artist", "Album", "01. T.flac")); err != nil {
		t.Errorf("a normal path was refused: %v", err)
	}
	if err := l.MkdirInside(filepath.Join(l.Root, "Escape", "Album"), 0o775, nil); !errors.Is(err, ErrOutsideLibrary) {
		t.Errorf("MkdirInside through a symlink: %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("something was created outside the root: %v", entries)
	}
}

func TestLayoutMkdirInsideSetsModeDespiteUmaskAndCallsFixOnEachNewFolder(t *testing.T) {
	l := mkLib(t)
	var fixed []string
	dir := filepath.Join(l.Root, "Artist", "Album")
	if err := l.MkdirInside(dir, 0o775, func(p string) error { fixed = append(fixed, filepath.Base(p)); return nil }); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil || fi.Mode().Perm() != 0o775 {
		t.Errorf("album folder mode = %v %v, want 775", fi.Mode().Perm(), err)
	}
	if strings.Join(fixed, ",") != "Artist,Album" {
		t.Errorf("fix called on %v, want each new folder", fixed)
	}
	fixed = nil
	if err := l.MkdirInside(dir, 0o775, func(p string) error { fixed = append(fixed, p); return nil }); err != nil || len(fixed) != 0 {
		t.Errorf("second MkdirInside = %v fixed %v, want a no-op", err, fixed)
	}
}

func TestLayoutNumberingContinuousAcrossDiscsAndRepairOfInsaneTags(t *testing.T) {
	disc1 := []layoutTrack{}
	for i := 1; i <= 12; i++ {
		disc1 = append(disc1, layoutTrack{Key: string(rune('a' + i)), Disc: 1, Track: i})
	}
	self := layoutTrack{Key: "enc", Disc: 2, Track: 4}
	n, repaired := layoutNumberFor(self, disc1)
	if repaired || n.Continuous != 16 || n.Disc != 2 || n.DiscTrack != 4 {
		t.Errorf("number = %+v repaired %v, want disc 2 track 4 -> 16", n, repaired)
	}
	// The 2026-10-11 WEB-FLAC: encore disc tagged disc 1, tracks 1-4, colliding with the main disc.
	bad := append(append([]layoutTrack{}, disc1[:3]...), layoutTrack{Key: "z1", Disc: 1, Track: 1}, layoutTrack{Key: "z2", Disc: 1, Track: 2})
	nums, repaired := layoutNumberAlbum(bad)
	if !repaired || len(nums) != 5 {
		t.Fatalf("collision not repaired: %+v %v", nums, repaired)
	}
	seen := map[int]bool{}
	for _, x := range nums {
		if x.Disc != 1 || seen[x.Continuous] {
			t.Errorf("repaired numbering has duplicates or a stray disc: %+v", nums)
		}
		seen[x.Continuous] = true
	}
	// No tag at all: disc 1 track 1, flagged as a fallback.
	if n, rep := layoutNumberFor(layoutTrack{Key: "yt"}, nil); !rep || n.Continuous != 1 {
		t.Errorf("untagged = %+v %v", n, rep)
	}
	// A sibling with a clashing tag must not renumber the track being placed.
	n, rep := layoutNumberFor(layoutTrack{Key: "me", Disc: 1, Track: 5}, []layoutTrack{{Key: "x", Disc: 1, Track: 5}})
	if !rep || n.Continuous != 5 {
		t.Errorf("clashing sibling: %+v %v, want own tagged position 5", n, rep)
	}
}
