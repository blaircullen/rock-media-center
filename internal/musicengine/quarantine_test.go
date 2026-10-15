package musicengine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type quarFix struct {
	t    *testing.T
	v    *env
	root string
	q    *Quarantine
}

func newQuarFix(t *testing.T) *quarFix {
	t.Helper()
	v := newEnv(t)
	root := filepath.Join(t.TempDir(), "quarantine")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	q, err := NewQuarantine(QuarantineConfig{DB: v.db, Root: root, Now: v.clock.Now, Share: func() int64 { return 1000 }})
	if err != nil {
		t.Fatal(err)
	}
	return &quarFix{t: t, v: v, root: root, q: q}
}

// put writes a quarantined version and its row, created age ago.
func (f *quarFix) put(track int64, name string, size int, age time.Duration) (path string, id int64) {
	f.t.Helper()
	path = f.q.PathFor(track, name, "abcdef0123456789", name+".flac")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", size)), 0o644); err != nil {
		f.t.Fatal(err)
	}
	res, err := f.v.db.Exec(`INSERT INTO mus_quarantine (track_id, path, origin_path, sha256, size_bytes, created_at) VALUES (NULL, ?, '/lib/x.flac', 'abc', ?, ?)`,
		path, size, f.v.clock.Now().Add(-age).Unix())
	if err != nil {
		f.t.Fatal(err)
	}
	id, _ = res.LastInsertId()
	return path, id
}

func (f *quarFix) cleared(id int64) bool {
	f.t.Helper()
	var at int64
	if err := f.v.db.QueryRow(`SELECT cleared_at FROM mus_quarantine WHERE id = ?`, id).Scan(&at); err != nil {
		f.t.Fatal(err)
	}
	return at != 0
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestQuarPathLayoutIsPerTrackAndUniquePerSwap(t *testing.T) {
	f := newQuarFix(t)
	a := f.q.PathFor(42, "swap-7", "0123456789abcdef", "/lib/Band/Album/01 - Song.flac")
	b := f.q.PathFor(42, "swap-8", "0123456789abcdef", "/lib/Band/Album/01 - Song.flac")
	if a == b {
		t.Fatal("two swaps of the same bytes share a quarantine path")
	}
	if want := filepath.Join(f.root, "42") + string(filepath.Separator); !strings.HasPrefix(a, want) {
		t.Errorf("path %s is not under the track's folder %s", a, want)
	}
	if !strings.HasSuffix(a, "swap-7-0123456789ab-01 - Song.flac") {
		t.Errorf("path %s should end with tag, short checksum and file name", a)
	}
	evil := f.q.PathFor(1, "../../x", "ab", "/lib/../../etc/passwd")
	if err := f.q.Inside(evil); err != nil {
		t.Errorf("a hostile name left the root: %s (%v)", evil, err)
	}
	if strings.Contains(strings.TrimPrefix(evil, f.root), "..") {
		t.Errorf("path %s still contains ..", evil)
	}
}

func TestQuarUsageCountsOnlyUnclearedVersions(t *testing.T) {
	f := newQuarFix(t)
	_, old := f.put(1, "a", 400, 100*24*time.Hour)
	f.put(1, "b", 700, time.Hour)
	u, err := f.q.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u.UsedBytes != 1100 || u.Versions != 2 || u.ShareBytes != 1000 || !u.Full {
		t.Fatalf("usage = %+v, want 1100 B in 2 versions, full against a 1000 B share", u)
	}
	if _, err := f.q.Clear(context.Background(), 90*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if !f.cleared(old) {
		t.Fatal("the old version was not cleared")
	}
	u, _ = f.q.Usage(context.Background())
	if u.UsedBytes != 700 || u.Versions != 1 || u.Full {
		t.Fatalf("usage after clear = %+v, want 700 B, not full", u)
	}
}

// R34: Clear removes only versions older than the age and marks their rows.
func TestQuarClearRemovesOnlyOldVersions(t *testing.T) {
	f := newQuarFix(t)
	oldP, oldID := f.put(1, "old", 100, 120*24*time.Hour)
	edgeP, edgeID := f.put(1, "edge", 100, 90*24*time.Hour) // exactly the age: cleared
	youngP, youngID := f.put(2, "young", 100, 89*24*time.Hour)
	rep, err := f.q.Clear(context.Background(), DefaultQuarantineClearAge)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Cleared != 2 || rep.FreedBytes != 200 {
		t.Errorf("report %+v, want 2 cleared and 200 B freed", rep)
	}
	if exists(oldP) || exists(edgeP) {
		t.Error("an old version is still on disk")
	}
	if !exists(youngP) || f.cleared(youngID) {
		t.Error("a version younger than the age was touched")
	}
	if !f.cleared(oldID) || !f.cleared(edgeID) {
		t.Error("cleared rows are not marked")
	}
	// The track's folder goes when it empties, and stays while it holds something.
	if exists(filepath.Dir(oldP)) {
		t.Error("the emptied track folder was left behind")
	}
	if !exists(filepath.Dir(youngP)) {
		t.Error("a non-empty track folder was removed")
	}
	// Clearing again changes nothing.
	again, err := f.q.Clear(context.Background(), DefaultQuarantineClearAge)
	if err != nil || again.Cleared != 0 {
		t.Errorf("second clear = %+v, %v, want nothing to do", again, err)
	}
}

func TestQuarClearAgeZeroClearsEverything(t *testing.T) {
	f := newQuarFix(t)
	p, id := f.put(1, "a", 10, time.Second)
	if rep, err := f.q.Clear(context.Background(), 0); err != nil || rep.Cleared != 1 || exists(p) || !f.cleared(id) {
		t.Fatalf("clear(0) = %+v, %v", rep, err)
	}
	if _, err := f.q.Clear(context.Background(), -time.Hour); err == nil {
		t.Fatal("a negative age was accepted")
	}
}

// Clear never touches anything outside the quarantine root, whatever a row says.
func TestQuarClearNeverTouchesAnythingOutsideTheRoot(t *testing.T) {
	f := newQuarFix(t)
	outside := t.TempDir()
	precious := filepath.Join(outside, "library", "precious.flac")
	if err := os.MkdirAll(filepath.Dir(precious), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(precious, []byte("irreplaceable"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 1: a row that names a library file directly.
	// 2: a row with ".." that resolves outside.
	// 3: a symlink inside the root that points at the library file.
	// 4: a sibling directory whose name starts with the root's name.
	sibling := f.root + "-sibling"
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	siblingFile := filepath.Join(sibling, "keep.flac")
	if err := os.WriteFile(siblingFile, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.root, "9", "link.flac")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(precious, link); err != nil {
		t.Fatal(err)
	}
	old := f.v.clock.Now().Add(-365 * 24 * time.Hour).Unix()
	paths := []string{precious, filepath.Join(f.root, "..", "library-escape.flac"), link, siblingFile}
	if err := os.WriteFile(filepath.Join(f.root, "..", "library-escape.flac"), []byte("escape"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if _, err := f.v.db.Exec(`INSERT INTO mus_quarantine (path, size_bytes, created_at) VALUES (?, 5, ?)`, p, old); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := f.q.Clear(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if rep.Cleared != 0 || len(rep.Skipped) != len(paths) {
		t.Errorf("report %+v, want everything skipped", rep)
	}
	for _, p := range []string{precious, filepath.Join(f.root, "..", "library-escape.flac"), siblingFile} {
		if b, err := os.ReadFile(p); err != nil || len(b) == 0 {
			t.Errorf("%s was damaged (%v)", p, err)
		}
	}
	if !exists(link) {
		t.Error("the symlink was removed")
	}
	if n := f.v.count(`SELECT COUNT(*) FROM mus_quarantine WHERE cleared_at <> 0`); n != 0 {
		t.Errorf("%d rows were marked cleared although nothing was removed", n)
	}
}

func TestQuarClearMarksAMissingFileClearedAndCountsIt(t *testing.T) {
	f := newQuarFix(t)
	p, id := f.put(1, "gone", 10, 200*24*time.Hour)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	rep, err := f.q.Clear(context.Background(), time.Hour)
	if err != nil || rep.Cleared != 1 || rep.Missing != 1 || !f.cleared(id) {
		t.Fatalf("report %+v, %v, want the row cleared and the missing file noted", rep, err)
	}
}

// A version an unsettled journal row still points at is kept.
func TestQuarClearKeepsVersionsAnOpenJournalRowNeeds(t *testing.T) {
	f := newQuarFix(t)
	p, id := f.put(1, "inflight", 10, 200*24*time.Hour)
	if _, err := f.v.db.Exec(`INSERT INTO mus_swap_journal (kind, new_path, quarantine_path, started_at, phase_at) VALUES ('swap', '/lib/x.flac', ?, 1, 1)`, p); err != nil {
		t.Fatal(err)
	}
	rep, err := f.q.Clear(context.Background(), time.Hour)
	if err != nil || rep.Cleared != 0 || !exists(p) || f.cleared(id) {
		t.Fatalf("report %+v, %v: a version in use by an open swap was cleared", rep, err)
	}
}

func TestQuarClearOnUnreachableRootChangesNothing(t *testing.T) {
	f := newQuarFix(t)
	p, id := f.put(1, "a", 10, 200*24*time.Hour)
	if err := os.Rename(f.root, f.root+"-away"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.Clear(context.Background(), time.Hour); err == nil {
		t.Fatal("clear succeeded with the root unreachable")
	}
	if f.cleared(id) {
		t.Fatal("a row was marked cleared with the root unreachable")
	}
	_ = p
}

func TestNewQuarantineNeedsAnAbsoluteRoot(t *testing.T) {
	v := newEnv(t)
	if _, err := NewQuarantine(QuarantineConfig{DB: v.db, Root: "relative/dir"}); err == nil {
		t.Error("relative root accepted")
	}
	if _, err := NewQuarantine(QuarantineConfig{Root: "/x"}); err == nil {
		t.Error("missing database accepted")
	}
}

// AE16: with quarantine at its share the sweep pauses while new music continues; clearing
// versions older than 90 days resumes the sweep, and undo of a cleared swap says so.
func TestQuarShareFullPausesSweepNotIntakeUntilCleared(t *testing.T) {
	r := newSwapHarness(t)
	inc := r.seedTrack(swapTrackOpts{Title: "Gold", Audio: "OLD-GOLD SUSPECT", RG: "rg1", Playlists: []string{"P"}})
	item, _ := r.seedSwap(inc, swapCandOpts{Audio: "NEW-GOLD"})
	r.drive(item, 6, StateDone)
	swapID := r.latestSwap()

	vols := &spaceFakeVolumes{}
	vols.set(1000, 1000)
	space, err := NewSpace(SpaceConfig{
		DB: r.v.db, LibraryPath: spaceLib, DownloadsPath: spaceDl,
		Settings: func() SpaceSettings {
			// A share the one quarantined version (a few hundred bytes) already fills.
			return SpaceSettings{Floor: spaceFloor, Margin: spaceMargin, QuarantineShare: 50}
		},
		Measure: vols.measure, MeasureTimeout: 2 * time.Second, Notifier: r.v.notifier, Now: r.v.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, why := space.Allow(context.Background(), WorkSweep); ok || !strings.Contains(why, "Replaced files") {
		t.Fatalf("sweep allowed = %v (%q), want paused because quarantine is full", ok, why)
	}
	if ok, why := space.Allow(context.Background(), WorkIntake); !ok {
		t.Fatalf("intake paused: %q; new music must continue", why)
	}
	// And the sweep itself opens nothing.
	other := r.seedTrack(swapTrackOpts{Title: "Next", Audio: "OLD-NEXT", Playlists: []string{"P"}})
	_ = other
	sw, _ := NewSweep(SweepConfig{DB: r.v.db, Space: space, Swaps: r.sw, Now: r.v.clock.Now})
	rep, err := sw.Run(context.Background())
	if err != nil || !rep.Paused || rep.Created != 0 {
		t.Fatalf("sweep run = %+v, %v, want paused with nothing opened", rep, err)
	}

	// Blair clears versions older than 90 days (the swap is 91 days old by now).
	r.v.clock.Advance(91 * 24 * time.Hour)
	crep, err := r.q.Clear(context.Background(), DefaultQuarantineClearAge)
	if err != nil || crep.Cleared != 1 {
		t.Fatalf("clear = %+v, %v, want the one version cleared", crep, err)
	}
	if ok, why := space.Allow(context.Background(), WorkSweep); !ok {
		t.Fatalf("sweep still paused after the clear: %q", why)
	}
	rep, err = sw.Run(context.Background())
	if err != nil || rep.Paused {
		t.Fatalf("sweep run after clear = %+v, %v, want it running", rep, err)
	}

	// Undo of the cleared swap is refused with that reason, and nothing changes.
	before := r.audioOf(inc.Local)
	_, uerr := r.sw.Undo(context.Background(), swapID)
	var ref *UndoRefusal
	if !errors.As(uerr, &ref) || !ref.Cleared || !strings.Contains(ref.Reason, "saved copy of the old file was deleted") {
		t.Fatalf("undo err = %v, want a refusal saying the old version was cleared", uerr)
	}
	if !errors.Is(uerr, ErrUndoRefused) {
		t.Error("the refusal does not match ErrUndoRefused")
	}
	if r.audioOf(inc.Local) != before || r.journalRows(JournalUndo) != 0 {
		t.Error("a refused undo changed something")
	}
}
