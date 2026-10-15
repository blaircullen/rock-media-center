package library

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// sparse makes a file of the given logical size without writing its bytes, so a
// "60 MB" video costs nothing on disk.
func sparse(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
}

func TestFindVideosCtxStopsAtMaxResults(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		sparse(t, filepath.Join(dir, fmt.Sprintf("Show.S01E0%d.1080p.mkv", i+1)), 60<<20)
	}
	sparse(t, filepath.Join(dir, "sample.mkv"), 60<<20)      // sample: skipped
	sparse(t, filepath.Join(dir, "Show.S01E09.mkv"), 10<<20) // under the floor: skipped

	all, truncated, err := FindVideosCtx(context.Background(), dir, 0, 0)
	if err != nil || truncated || len(all) != 5 {
		t.Fatalf("unbounded: %d files, truncated %v, err %v; want 5, false", len(all), truncated, err)
	}
	got, truncated, err := FindVideosCtx(context.Background(), dir, 3, 0)
	if err != nil || !truncated || len(got) != 3 {
		t.Errorf("capped at 3: %d files, truncated %v, err %v", len(got), truncated, err)
	}
	// Exactly at the cap is not "cut short".
	if got, truncated, _ := FindVideosCtx(context.Background(), dir, 5, 0); truncated || len(got) != 5 {
		t.Errorf("cap == count: %d files, truncated %v", len(got), truncated)
	}
	// The old entry point is unchanged.
	if old, err := FindVideos(dir); err != nil || len(old) != 5 {
		t.Errorf("FindVideos: %d, %v", len(old), err)
	}
}

func TestFindVideosCtxRespectsMaxVisited(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 10; i++ {
		sparse(t, filepath.Join(dir, fmt.Sprintf("d%02d", i), "Film.mkv"), 60<<20)
	}
	got, truncated, err := FindVideosCtx(context.Background(), dir, 0, 6)
	if err != nil || !truncated || len(got) >= 10 {
		t.Errorf("visit cap 6: %d files, truncated %v, err %v", len(got), truncated, err)
	}
}

func TestFindVideosCtxCancelled(t *testing.T) {
	dir := t.TempDir()
	sparse(t, filepath.Join(dir, "Film.mkv"), 60<<20)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, _, err := FindVideosCtx(ctx, dir, 0, 0)
	if !errors.Is(err, context.Canceled) || len(got) != 0 {
		t.Errorf("cancelled walk: %d files, err %v", len(got), err)
	}
}
