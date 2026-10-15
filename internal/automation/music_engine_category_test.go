package automation

import (
	"testing"

	"github.com/tristenlammi/arrmada/internal/download"
	"github.com/tristenlammi/arrmada/internal/music"
)

// The lossless music engine files its downloads under rmc-music (plan KTD1). The album
// importer reads only arrmada-music, so a finished engine download is never taken, held for
// review or counted as an unmatched album, while the same download in the album category
// is picked up by the very same cycles.
func TestAlbumImporterIgnoresMusicEngineDownloads(t *testing.T) {
	h := newLifecycleHarness(t)
	ctx := h.ctx
	h.c.music = music.NewService(h.c.db, nil, h.c.log)
	mustExec(t, h.c, `INSERT INTO artists (id, mbid, name) VALUES (1, 'a1', 'Radiohead')`)
	mustExec(t, h.c, `INSERT INTO albums (id, artist_id, mbid, title, year) VALUES (1, 1, 'r1', 'Kid A', 2000)`)

	dir := h.download(t, "Radiohead - Kid A (2000) [FLAC]")
	engineHash := h.completedTorrent("Radiohead - Kid A (2000) [FLAC]", download.CategoryMusicEngine, dir)
	for i := 0; i < unmatchedReviewAfter+1; i++ {
		h.c.ImportMusicDownloads(ctx)
	}
	var n int
	if err := h.c.db.QueryRow(`SELECT COUNT(*) FROM import_reviews WHERE hash = ?`, engineHash).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("the album importer opened %d review(s) for a download in %q", n, download.CategoryMusicEngine)
	}

	// Control: the same kind of download in the album category is handled by these cycles.
	dir2 := h.download(t, "Radiohead - Kid A (2000) [FLAC] x")
	albumHash := h.completedTorrent("Radiohead - Kid A (2000) [FLAC] x", download.CategoryMusic, dir2)
	for i := 0; i < unmatchedReviewAfter+1; i++ {
		h.c.ImportMusicDownloads(ctx)
	}
	if r := reviewFor(t, h.c, albumHash); r.Hash != albumHash {
		t.Fatalf("control download in %q not picked up", download.CategoryMusic)
	}
}
