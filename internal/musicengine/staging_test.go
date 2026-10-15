package musicengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func stagingWrite(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func stagingExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// stagingCandidate inserts a candidate row with a staged file and returns the file path.
func (h *acqHarness) stagingCandidate(itemID int64, state string, expiresAt int64) (candID int64, file string) {
	h.t.Helper()
	res, err := h.db.Exec(
		`INSERT INTO mus_candidates (item_id, lane, state, release_name, created_at, expires_at) VALUES (?, 'usenet', ?, 'r', ?, ?)`,
		itemID, state, h.clock.Now().Unix(), expiresAt)
	if err != nil {
		h.t.Fatal(err)
	}
	candID, _ = res.LastInsertId()
	file = filepath.Join(h.staging.CandidateDir(itemID, candID), "rel", "02 - Hang With Me.flac")
	stagingWrite(h.t, file)
	return candID, file
}

// Staging files older than 7 days are removed; younger ones stay (R10).
func TestStagingExpiryRemovesOldKeepsYoung(t *testing.T) {
	h := newAcqHarness(t)
	id := h.newItem(KindRequest, StateWaitingBlair)
	now := h.clock.Now()
	_, old := h.stagingCandidate(id, "rejected", now.Add(-time.Minute).Unix())
	_, young := h.stagingCandidate(id, "rejected", now.Add(StagingExpiry-time.Hour).Unix())

	rep, err := h.acq.ExpireStaging(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stagingExists(old) {
		t.Error("expired rejected candidate was not removed")
	}
	if !stagingExists(young) {
		t.Error("a candidate inside its 7 days was removed")
	}
	if rep.RemovedCandidates != 1 {
		t.Errorf("report = %+v", rep)
	}
	if n := h.count(`SELECT COUNT(*) FROM mus_candidates WHERE staging_path = '' AND expires_at = 0 AND state = 'rejected'`); n != 1 {
		t.Errorf("expired row should be cleared exactly once, got %d", n)
	}

	// A week later the young one goes too.
	h.clock.Advance(StagingExpiry)
	if _, err := h.acq.ExpireStaging(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stagingExists(young) {
		t.Error("candidate past its 7 days was not removed")
	}
}

// The release a chosen candidate came from stays while its item is live (it may be waiting
// for Blair, KTD12) and leaves once the item has finished and the expiry has passed.
func TestStagingExpiryKeepsChosenCandidateOfLiveItem(t *testing.T) {
	h := newAcqHarness(t)
	id := h.newItem(KindRequest, StateWaitingBlair)
	_, file := h.stagingCandidate(id, "chosen", h.clock.Now().Add(-time.Hour).Unix())

	if _, err := h.acq.ExpireStaging(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !stagingExists(file) {
		t.Fatal("a staged candidate of a live item was removed")
	}
	if _, err := h.db.Exec(`UPDATE mus_items SET state = 'done' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := h.acq.ExpireStaging(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stagingExists(file) {
		t.Fatal("the remainder of a finished item's release was not removed after expiry")
	}
}

// A directory no candidate owns is removed once it is older than the expiry, by mtime.
func TestStagingExpirySweepsOldOrphans(t *testing.T) {
	h := newAcqHarness(t)
	oldF := filepath.Join(h.staging.Root(), "900", "901", "leftover.flac")
	newF := filepath.Join(h.staging.Root(), "900", "902", "fresh.flac")
	stagingWrite(t, oldF)
	stagingWrite(t, newF)
	old := h.clock.Now().Add(-StagingExpiry - time.Hour)
	if err := os.Chtimes(filepath.Dir(oldF), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Dir(newF), h.clock.Now(), h.clock.Now()); err != nil {
		t.Fatal(err)
	}

	rep, err := h.acq.ExpireStaging(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stagingExists(oldF) || !stagingExists(newF) || rep.RemovedOrphans != 1 {
		t.Errorf("old exists=%v new exists=%v report=%+v", stagingExists(oldF), stagingExists(newF), rep)
	}
}

// Nothing under the library root is ever removed, whatever a row or a link says.
func TestStagingExpiryNeverTouchesLibrary(t *testing.T) {
	h := newAcqHarness(t)
	id := h.newItem(KindRequest, StateWaitingBlair)
	libFile := filepath.Join(h.libRoot, "Robyn", "Body Talk", "01. Hang With Me.flac")
	stagingWrite(t, libFile)

	// 1. A bad row: staging_path points into the library. Expiry works from ids, not that path.
	candID, _ := h.stagingCandidate(id, "rejected", h.clock.Now().Add(-time.Minute).Unix())
	if _, err := h.db.Exec(`UPDATE mus_candidates SET staging_path = ? WHERE id = ?`, filepath.Dir(libFile), candID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.acq.ExpireStaging(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !stagingExists(libFile) {
		t.Fatal("expiry removed a library file named by a candidate row")
	}

	// 2. A symlink in staging that points at the library: the removal is refused.
	candID2, _ := h.stagingCandidate(id, "rejected", h.clock.Now().Add(-time.Minute).Unix())
	dir := h.staging.CandidateDir(id, candID2)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(h.libRoot, dir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, err := h.acq.ExpireStaging(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if !stagingExists(libFile) {
		t.Fatal("expiry followed a symlink into the library")
	}

	// 3. Direct: removeDir refuses the library, the staging root and paths outside both.
	for _, p := range []string{h.libRoot, libFile, h.staging.Root(), filepath.Dir(h.staging.Root()), "relative/path"} {
		if err := h.staging.removeDir(p); err == nil {
			t.Errorf("removeDir(%q) succeeded", p)
		}
	}
	if !stagingExists(libFile) {
		t.Fatal("library file gone")
	}
}

func TestNewStagingRejectsOverlapWithLibrary(t *testing.T) {
	lib := t.TempDir()
	for name, root := range map[string]string{
		"inside library": filepath.Join(lib, "staging"),
		"equal":          lib,
		"contains":       filepath.Dir(lib),
		"relative":       "staging",
		"empty":          "",
	} {
		if _, err := NewStaging(root, lib); err == nil {
			t.Errorf("%s: NewStaging accepted root %q", name, root)
		}
	}
	if _, err := NewStaging(t.TempDir()); err == nil {
		t.Error("NewStaging without a library root should fail")
	}
	if _, err := NewStaging(filepath.Join(t.TempDir(), "s"), lib); err != nil {
		t.Errorf("separate roots rejected: %v", err)
	}
}

func TestStagingFindTrack(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{
		"01 - Include Me Out.flac",
		"02 - Hang With Me (Live).flac",
		"03 - Hang With Me (Remix).flac",
		"cover.jpg",
	} {
		stagingWrite(t, filepath.Join(dir, "disc", f))
	}
	if _, err := stagingFindTrack(dir, "Hang With Me", "studio"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("studio request matched a live or remix file: %v", err)
	}
	got, err := stagingFindTrack(dir, "Hang With Me", "live")
	if err != nil || !strings.HasSuffix(got, "(Live).flac") {
		t.Fatalf("live request = %q, %v", got, err)
	}
	stagingWrite(t, filepath.Join(dir, "disc", "02 - Hang With Me.flac"))
	got, err = stagingFindTrack(dir, "Hang With Me", "")
	if err != nil || filepath.Base(got) != "02 - Hang With Me.flac" {
		t.Fatalf("studio request = %q, %v", got, err)
	}
	// A title that is itself a number still matches past the track number.
	stagingWrite(t, filepath.Join(dir, "t", "04 - 22.flac"))
	if got, err := stagingFindTrack(filepath.Join(dir, "t"), "22", "studio"); err != nil || filepath.Base(got) != "04 - 22.flac" {
		t.Fatalf("numeric title = %q, %v", got, err)
	}
}
