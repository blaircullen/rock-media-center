package metadata

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const isrcBody = `{"isrc":"SEXXX1000001","recordings":[
 {"id":"rec-studio","title":"Hang With Me","length":210000,"disambiguation":"",
  "artist-credit":[{"name":"Robyn","joinphrase":"","artist":{"id":"art-1","name":"Robyn"}}],
  "releases":[{"id":"rel-1","title":"Body Talk Pt. 2","status":"Official","date":"2010-09-06","country":"XW",
    "release-group":{"id":"rg-1","title":"Body Talk Pt. 2","primary-type":"EP","secondary-types":[]}}]},
 {"id":"rec-live","title":"Springsteen","length":251000,"disambiguation":"live at the Ryman, acoustic",
  "artist-credit":[{"name":"Eric Church","joinphrase":"","artist":{"id":"art-2","name":"Eric Church"}}],
  "releases":[{"id":"rel-2","title":"Live","status":"Official","date":"2017-01-01",
    "release-group":{"id":"rg-2","title":"Live","primary-type":"Album","secondary-types":["Live"]}}]}
]}`

// serveISRC answers like MusicBrainz: the isrc resource names the recordings (it rejects
// inc=release-groups with 400), and the recording resource returns each one in full.
func serveISRC(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var all struct {
		Recordings []json.RawMessage `json:"recordings"`
	}
	if err := json.Unmarshal([]byte(isrcBody), &all); err != nil {
		t.Fatal(err)
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/isrc/"):
		if strings.Contains(r.URL.Query().Get("inc"), "release-groups") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"release-groups is not a valid inc parameter for the isrc resource."}`)
			return
		}
		var ids []string
		for _, raw := range all.Recordings {
			var rec struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(raw, &rec)
			ids = append(ids, `{"id":"`+rec.ID+`"}`)
		}
		_, _ = io.WriteString(w, `{"recordings":[`+strings.Join(ids, ",")+`]}`)
	case strings.HasPrefix(r.URL.Path, "/recording/"):
		id := strings.TrimPrefix(r.URL.Path, "/recording/")
		for _, raw := range all.Recordings {
			if strings.Contains(string(raw), `"id":"`+id+`"`) {
				_, _ = w.Write(raw)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	default:
		_, _ = io.WriteString(w, `{}`)
	}
}

func TestISRCLookupReturnsRecordingAndReleases(t *testing.T) {
	var path, inc string
	m := mbStub(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/isrc/") {
			path, inc = r.URL.Path, r.URL.Query().Get("inc")
		}
		serveISRC(t, w, r)
	})
	recs, err := m.RecordingsByISRC(context.Background(), "sexxx1000001")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/isrc/SEXXX1000001" {
		t.Errorf("path = %q", path)
	}
	if strings.Contains(inc, "release-groups") {
		t.Errorf("inc = %q: the isrc resource rejects release-groups", inc)
	}
	if len(recs) != 2 {
		t.Fatalf("recordings = %d", len(recs))
	}
	r := recs[0]
	if r.MBID != "rec-studio" || r.Artist != "Robyn" || r.LengthMs != 210000 || len(r.ArtistMBIDs) != 1 {
		t.Errorf("recording = %+v", r)
	}
	if len(r.Releases) != 1 || r.Releases[0].MBID != "rel-1" || r.Releases[0].ReleaseGroupMBID != "rg-1" ||
		r.Releases[0].PrimaryType != "EP" {
		t.Errorf("releases = %+v", r.Releases)
	}
	if len(r.Versions) != 0 {
		t.Errorf("studio recording should carry no version hints, got %v", r.Versions)
	}
}

// AE4: a live recording must be recognisable as live so it never counts as the studio one.
func TestLiveRecordingReportsLiveDisambiguation(t *testing.T) {
	m := mbStub(t, func(w http.ResponseWriter, r *http.Request) { serveISRC(t, w, r) })
	recs, err := m.RecordingsByISRC(context.Background(), "SEXXX1000001")
	if err != nil {
		t.Fatal(err)
	}
	live := recs[1]
	if live.Disambiguation != "live at the Ryman, acoustic" {
		t.Errorf("disambiguation = %q", live.Disambiguation)
	}
	if strings.Join(live.Versions, ",") != "live,acoustic" {
		t.Errorf("versions = %v, want [live acoustic]", live.Versions)
	}
	if got := live.Releases[0].Versions; len(got) != 1 || got[0] != "live" {
		t.Errorf("release-group secondary type hint = %v, want [live]", got)
	}
}

func TestISRCNotFoundIsEmptyNotError(t *testing.T) {
	m := mbStub(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	recs, err := m.RecordingsByISRC(context.Background(), "ZZZZZ0000000")
	if err != nil || len(recs) != 0 {
		t.Errorf("got %v, %v; want empty and nil", recs, err)
	}
}

func TestGetRecordingAndRelease(t *testing.T) {
	m := mbStub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/recording/rec-1":
			if r.URL.Query().Get("inc") != "releases+release-groups+artist-credits+isrcs" {
				t.Errorf("recording inc = %q", r.URL.Query().Get("inc"))
			}
			_, _ = io.WriteString(w, `{"id":"rec-1","title":"Song","length":1000,"disambiguation":"radio edit","isrcs":["AA1"],"artist-credit":[{"name":"A","joinphrase":" feat. ","artist":{"id":"a"}},{"name":"B","joinphrase":"","artist":{"id":"b"}}],"releases":[]}`)
		case "/release/rel-1":
			if r.URL.Query().Get("inc") != "recordings+media+release-groups+artist-credits" {
				t.Errorf("release inc = %q", r.URL.Query().Get("inc"))
			}
			_, _ = io.WriteString(w, `{"id":"rel-1","title":"Album","status":"Official","release-group":{"id":"rg","primary-type":"Album","secondary-types":["Remix"]},
			 "artist-credit":[{"name":"A","joinphrase":"","artist":{"id":"a"}}],
			 "media":[{"position":2,"tracks":[{"title":"T1","position":1,"length":0,"recording":{"id":"r1","length":99000,"disambiguation":"demo"}}]}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	rec, err := m.GetRecording(context.Background(), "rec-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Artist != "A feat. B" || len(rec.ISRCs) != 1 || rec.Versions[0] != "edit" {
		t.Errorf("recording = %+v", rec)
	}
	rel, err := m.GetRelease(context.Background(), "rel-1")
	if err != nil {
		t.Fatal(err)
	}
	if rel.ReleaseGroupMBID != "rg" || rel.Artist != "A" || len(rel.Tracks) != 1 {
		t.Fatalf("release = %+v", rel)
	}
	tr := rel.Tracks[0]
	if tr.Disc != 2 || tr.Position != 1 || tr.LengthMs != 99000 || tr.RecordingMBID != "r1" || tr.Versions[0] != "demo" {
		t.Errorf("track = %+v", tr)
	}
	if rel.Versions[0] != "remix" {
		t.Errorf("release versions = %v", rel.Versions)
	}
}

func TestSearchRecordingsBuildsQueryAndOrdersByScore(t *testing.T) {
	var q string
	m := mbStub(t, func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.Query().Get("query")
		_, _ = io.WriteString(w, `{"recordings":[{"id":"low","title":"x","score":40},{"id":"high","title":"x","score":100}]}`)
	})
	recs, err := m.SearchRecordings(context.Background(), `Rob"yn`, "Hang With Me", 210000)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`recording:"Hang With Me"`, `artist:"Rob\"yn"`, `dur:[205000 TO 215000]`} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q missing %q", q, want)
		}
	}
	if recs[0].MBID != "high" {
		t.Errorf("order = %v", recs)
	}
}

// An album-module (catalogue) lookup queued beside an intake lookup must wait for it.
func TestCatalogueLookupWaitsForIntakeLookup(t *testing.T) {
	var (
		mu    sync.Mutex
		order []string
	)
	m := mbStub(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		order = append(order, r.URL.Path)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"artists":[],"recordings":[]}`)
	})
	m.minInterval = 50 * time.Millisecond
	// Occupy the current slot so both real requests have to queue.
	if _, err := m.SearchArtists(context.Background(), "warmup"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	order = nil
	mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = m.SearchArtists(context.Background(), "album module") // catalogue, queued first
	}()
	time.Sleep(10 * time.Millisecond)
	go func() {
		defer wg.Done()
		_, _ = m.RecordingsByISRC(WithMBClass(context.Background(), MBIntake), "SEXXX1000001")
	}()
	wg.Wait()
	if len(order) != 2 || order[0] != "/isrc/SEXXX1000001" || order[1] != "/artist" {
		t.Errorf("request order = %v, want the intake ISRC lookup first", order)
	}
}

// The 503 retry/backoff survives the pacer swap, for the new lookups too.
func TestRecordingLookupRetries503(t *testing.T) {
	var calls int
	m := mbStub(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/isrc/") {
			calls++
			if calls < 3 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		}
		serveISRC(t, w, r)
	})
	recs, err := m.RecordingsByISRC(context.Background(), "SEXXX1000001")
	if err != nil || len(recs) != 2 || calls != 3 {
		t.Errorf("recs=%d err=%v calls=%d", len(recs), err, calls)
	}
}

// Rate limit at the HTTP layer, across classes: no two requests inside one slot.
func TestRequestsNeverCloserThanOneSlot(t *testing.T) {
	var (
		mu     sync.Mutex
		stamps []time.Time
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		stamps = append(stamps, time.Now())
		mu.Unlock()
		_, _ = io.WriteString(w, `{"artists":[],"recordings":[]}`)
	}))
	t.Cleanup(srv.Close)
	m := NewMusicBrainz()
	m.http, m.base = srv.Client(), srv.URL
	m.minInterval = 40 * time.Millisecond

	var wg sync.WaitGroup
	for i, c := range []MBClass{MBCatalogue, MBIntake, MBSweep, MBCatalogue, MBIntake} {
		wg.Add(1)
		go func(i int, c MBClass) {
			defer wg.Done()
			_, _ = m.SearchArtists(WithMBClass(context.Background(), c), "q")
		}(i, c)
	}
	wg.Wait()
	if len(stamps) != 5 {
		t.Fatalf("requests = %d", len(stamps))
	}
	for i := 1; i < len(stamps); i++ {
		if gap := stamps[i].Sub(stamps[i-1]); gap < 35*time.Millisecond {
			t.Errorf("requests %d and %d only %v apart, slot is 40ms", i-1, i, gap)
		}
	}
}
