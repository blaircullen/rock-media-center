package spotify

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// plFake is a playlist served in pages. Knobs are set before a read; metaCalls counts the
// GET /v1/playlists/{id} calls so a test can change the snapshot between before and after.
type plFake struct {
	f          *fakeSpotify
	mu         sync.Mutex
	items      []map[string]any
	reportTot  int // total reported; defaults to len(items)
	snapshots  []string
	metaCalls  int
	failOffset int    // item path answers 500 for this offset (-1 = never)
	itemsKey   string // "item" (current) or "track" (legacy)
	itemsPath  string
	noItems    bool // metadata has no items object (Dev Mode, not owner)
	forbidden  bool // items path answers 403
	nextHost   string
	lastQuery  []string
}

func newPlFake(t *testing.T, n int) (*plFake, *fakeSpotify) {
	f := newFake(t)
	p := &plFake{f: f, failOffset: -1, itemsKey: "item", itemsPath: "items", snapshots: []string{"snapA"}}
	for i := 0; i < n; i++ {
		p.items = append(p.items, trackItem(i))
	}
	p.reportTot = -1
	f.handler = p.serve
	return p, f
}

func trackItem(i int) map[string]any {
	return map[string]any{
		"added_at": "2026-01-01T00:00:00Z",
		"is_local": false,
		"TRACK": map[string]any{
			"id": fmt.Sprintf("trk%03d", i), "type": "track", "name": fmt.Sprintf("Song %d", i),
			"duration_ms": 200000 + i, "explicit": i%2 == 0,
			"artists":      []map[string]any{{"name": "Artist A"}, {"name": "Artist B"}},
			"album":        map[string]any{"name": "Album " + strconv.Itoa(i)},
			"external_ids": map[string]any{"isrc": fmt.Sprintf("usabc26%05d", i)},
		},
	}
}

func (p *plFake) total() int {
	if p.reportTot >= 0 {
		return p.reportTot
	}
	return len(p.items)
}

func (p *plFake) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastQuery = append(p.lastQuery, r.URL.String())
	w.Header().Set("Content-Type", "application/json")
	base := "/v1/playlists/PL1"
	switch r.URL.Path {
	case base:
		snap := p.snapshots[min(p.metaCalls, len(p.snapshots)-1)]
		p.metaCalls++
		m := map[string]any{"snapshot_id": snap, "name": "My Shazam Tracks", "owner": map[string]any{"id": "sunygxc"}}
		if !p.noItems {
			key := "items"
			if p.itemsKey == "track" {
				key = "tracks"
			}
			m[key] = map[string]any{"total": p.total()}
		}
		_ = json.NewEncoder(w).Encode(m)
	case base + "/" + p.itemsPath:
		if p.forbidden {
			http.Error(w, `{"error":{"status":403,"message":"Forbidden"}}`, http.StatusForbidden)
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		if off == p.failOffset {
			http.Error(w, `{"error":{"status":500,"message":"boom"}}`, http.StatusInternalServerError)
			return
		}
		end := min(off+limit, len(p.items))
		page := []map[string]any{}
		for _, it := range p.items[off:end] {
			c := map[string]any{}
			for k, v := range it {
				if k == "TRACK" {
					c[p.itemsKey] = v
				} else {
					c[k] = v
				}
			}
			page = append(page, c)
		}
		var next any
		if end < len(p.items) {
			host := p.nextHost
			if host == "" {
				host = "http://" + r.Host
			}
			next = fmt.Sprintf("%s%s/%s?offset=%d&limit=%d&market=%s", host, base, p.itemsPath, end, limit, r.URL.Query().Get("market"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"offset": off, "limit": limit, "total": p.total(), "next": next, "items": page})
	default:
		http.NotFound(w, r)
	}
}

func TestThreePagePlaylistReturnsEveryEntryInOrder(t *testing.T) {
	p, f := newPlFake(t, 7)
	c := f.client(t, func(cfg *Config) { cfg.PageSize = 3 })
	pl, err := c.ReadPlaylist(t.Context(), "PL1")
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Entries) != 7 || pl.Total != 7 || pl.Name != "My Shazam Tracks" || pl.OwnerID != "sunygxc" || pl.SnapshotID != "snapA" {
		t.Fatalf("playlist = %+v", pl.PlaylistInfo)
	}
	for i, e := range pl.Entries {
		if e.Position != i || e.TrackID != fmt.Sprintf("trk%03d", i) {
			t.Fatalf("entry %d = %+v", i, e)
		}
	}
	e := pl.Entries[2]
	if e.ISRC != "USABC2600002" || e.Name != "Song 2" || e.Album != "Album 2" || e.DurationMs != 200002 ||
		!e.Explicit || strings.Join(e.Artists, "|") != "Artist A|Artist B" || e.Unavailable || e.LinkedFromID != "" {
		t.Fatalf("entry fields = %+v", e)
	}
	if p.metaCalls != 2 {
		t.Fatalf("snapshot read %d times, want before and after (2)", p.metaCalls)
	}
	// 3 pages were fetched, each with the configured market.
	pages := 0
	for _, q := range p.lastQuery {
		if strings.Contains(q, "/items") {
			pages++
			if !strings.Contains(q, "market=US") {
				t.Fatalf("page request %q lacks market=US", q)
			}
		}
	}
	if pages != 3 {
		t.Fatalf("pages = %d, want 3", pages)
	}
}

// AE12: a 500 on page two returns an error and no entries.
func TestServerErrorOnPageTwoFailsTheWholeRead(t *testing.T) {
	p, f := newPlFake(t, 7)
	p.failOffset = 3
	c := f.client(t, func(cfg *Config) { cfg.PageSize = 3 })
	pl, err := c.ReadPlaylist(t.Context(), "PL1")
	if pl != nil || err == nil {
		t.Fatalf("pl=%v err=%v, want nil + error", pl, err)
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 500 || !strings.Contains(ae.Body, "boom") {
		t.Fatalf("err = %v, want APIError 500 with body kept", err)
	}
}

// The snapshot id changes between the before and after reads while the count stays equal.
func TestSnapshotChangeFailsEvenWithSameCount(t *testing.T) {
	p, f := newPlFake(t, 4)
	p.snapshots = []string{"snapA", "snapB"}
	c := f.client(t, func(cfg *Config) { cfg.PageSize = 2 })
	pl, err := c.ReadPlaylist(t.Context(), "PL1")
	if pl != nil || !errors.Is(err, ErrIncomplete) || !strings.Contains(err.Error(), "snapA -> snapB") {
		t.Fatalf("pl=%v err=%v, want ErrIncomplete naming both snapshots", pl, err)
	}
}

// Spotify says 5 but the pages hold 3.
func TestFewerItemsThanTotalFails(t *testing.T) {
	p, f := newPlFake(t, 3)
	p.reportTot = 5
	c := f.client(t, nil)
	pl, err := c.ReadPlaylist(t.Context(), "PL1")
	if pl != nil || !errors.Is(err, ErrIncomplete) {
		t.Fatalf("pl=%v err=%v, want ErrIncomplete", pl, err)
	}
}

func TestLocalFileEntryIsFlaggedWithNoISRC(t *testing.T) {
	p, f := newPlFake(t, 1)
	p.items = append(p.items, map[string]any{
		"added_at": "2026-01-02T00:00:00Z", "is_local": true,
		"TRACK": map[string]any{"id": nil, "type": "track", "name": "My Demo", "is_local": true,
			"artists": []map[string]any{{"name": "Me"}}, "album": map[string]any{"name": ""}, "duration_ms": 1000},
	})
	c := f.client(t, nil)
	pl, err := c.ReadPlaylist(t.Context(), "PL1")
	if err != nil {
		t.Fatal(err)
	}
	e := pl.Entries[1]
	if !e.IsLocal || e.ISRC != "" || e.TrackID != "" || e.Name != "My Demo" {
		t.Fatalf("local entry = %+v", e)
	}
}

func TestRelinkedTrackReturnsBothIDs(t *testing.T) {
	p, f := newPlFake(t, 1)
	p.items[0]["TRACK"].(map[string]any)["linked_from"] = map[string]any{"id": "origID", "type": "track"}
	c := f.client(t, nil)
	pl, err := c.ReadPlaylist(t.Context(), "PL1")
	if err != nil {
		t.Fatal(err)
	}
	if e := pl.Entries[0]; e.TrackID != "trk000" || e.LinkedFromID != "origID" {
		t.Fatalf("relinked entry = %+v", e)
	}
}

// A removed track comes back as a null item; it counts toward the total and is marked
// unavailable rather than dropped (dropping it would fail the count check).
func TestNullItemIsUnavailableEntry(t *testing.T) {
	p, f := newPlFake(t, 2)
	p.items[1]["TRACK"] = nil
	c := f.client(t, nil)
	pl, err := c.ReadPlaylist(t.Context(), "PL1")
	if err != nil {
		t.Fatal(err)
	}
	if e := pl.Entries[1]; !e.Unavailable || e.TrackID != "" || e.Position != 1 {
		t.Fatalf("null entry = %+v", e)
	}
}

// The pre-2026 shape (tracks / track, /tracks path) still reads when LegacyTracks is set.
func TestLegacyTracksShape(t *testing.T) {
	p, f := newPlFake(t, 3)
	p.itemsKey, p.itemsPath = "track", "tracks"
	c := f.client(t, func(cfg *Config) { cfg.LegacyTracks = true; cfg.PageSize = 2 })
	pl, err := c.ReadPlaylist(t.Context(), "PL1")
	if err != nil || len(pl.Entries) != 3 || pl.Entries[2].ISRC != "USABC2600002" {
		t.Fatalf("pl=%v err=%v", pl, err)
	}
}

// Development Mode: the app token gets metadata with no items object. That is a failed read.
func TestMetadataOnlyPlaylistIsContentsUnavailable(t *testing.T) {
	p, f := newPlFake(t, 3)
	p.noItems = true
	c := f.client(t, nil)
	pl, err := c.ReadPlaylist(t.Context(), "PL1")
	if pl != nil || !errors.Is(err, ErrContentsUnavailable) {
		t.Fatalf("pl=%v err=%v, want ErrContentsUnavailable", pl, err)
	}
}

func TestItems403IsContentsUnavailableWithBody(t *testing.T) {
	p, f := newPlFake(t, 3)
	p.forbidden = true
	c := f.client(t, nil)
	_, err := c.ReadPlaylist(t.Context(), "PL1")
	if !errors.Is(err, ErrContentsUnavailable) || !strings.Contains(err.Error(), "Forbidden") {
		t.Fatalf("err = %v, want ErrContentsUnavailable keeping the 403 body", err)
	}
}

// A next link to another host must not receive the bearer token.
func TestNextLinkToOtherHostRefused(t *testing.T) {
	p, f := newPlFake(t, 4)
	p.nextHost = "http://evil.example"
	c := f.client(t, func(cfg *Config) { cfg.PageSize = 2 })
	pl, err := c.ReadPlaylist(t.Context(), "PL1")
	if pl != nil || err == nil || !strings.Contains(err.Error(), "evil.example") {
		t.Fatalf("pl=%v err=%v, want refusal naming the host", pl, err)
	}
}

func TestEmptyPlaylistWithTotalZeroIsValid(t *testing.T) {
	_, f := newPlFake(t, 0)
	c := f.client(t, nil)
	pl, err := c.ReadPlaylist(t.Context(), "PL1")
	if err != nil || pl == nil || len(pl.Entries) != 0 || pl.Total != 0 {
		t.Fatalf("pl=%v err=%v", pl, err)
	}
}

func TestParsePlaylistID(t *testing.T) {
	for in, want := range map[string]string{
		"5hLOEmkXHaXQDDbvoFFLX8":                                        "5hLOEmkXHaXQDDbvoFFLX8",
		"spotify:playlist:5hLOEmkXHaXQDDbvoFFLX8":                       "5hLOEmkXHaXQDDbvoFFLX8",
		"https://open.spotify.com/playlist/5hLOEmkXHaXQDDbvoFFLX8?si=x": "5hLOEmkXHaXQDDbvoFFLX8",
	} {
		if got, err := ParsePlaylistID(in); err != nil || got != want {
			t.Errorf("ParsePlaylistID(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "a/b", "../x", "https://open.spotify.com/user/x"} {
		if _, err := ParsePlaylistID(bad); err == nil {
			t.Errorf("ParsePlaylistID(%q) succeeded, want error", bad)
		}
	}
}
