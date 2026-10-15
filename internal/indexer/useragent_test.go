package indexer

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/tristenlammi/arrmada/internal/store"
)

// uaRecorder is an indexer server that records the User-Agent of every request, by path.
type uaRecorder struct {
	mu   sync.Mutex
	seen map[string][]string
}

func newUARecorder(t *testing.T) (*uaRecorder, *httptest.Server) {
	t.Helper()
	rec := &uaRecorder{seen: map[string][]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.seen[r.URL.Path] = append(rec.seen[r.URL.Path], r.Header.Get("User-Agent"))
		rec.mu.Unlock()
		switch r.URL.Path {
		case "/getnzb":
			_, _ = w.Write([]byte(nzbBody))
		default:
			if r.URL.Query().Get("t") == "caps" {
				_, _ = w.Write([]byte(`<caps><searching><search available="yes"/></searching></caps>`))
				return
			}
			_, _ = w.Write([]byte(newznabSample))
		}
	}))
	t.Cleanup(srv.Close)
	return rec, srv
}

func (r *uaRecorder) last(path string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v := r.seen[path]; len(v) > 0 {
		return v[len(v)-1]
	}
	return "<no request>"
}

// A User-Agent set on a new indexer survives a reload and a store restart.
func TestUserAgentSurvivesReloadAndRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	repo := NewRepo(st.DB())
	created, err := repo.Create(ctx, Indexer{Name: "Geek", Kind: KindNewznab, URL: "http://x/api", UserAgent: "Arrmada/1.0 (+test)", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.Get(ctx, created.ID); got.UserAgent != "Arrmada/1.0 (+test)" {
		t.Fatalf("after reload UserAgent = %q", got.UserAgent)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st2, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st2.Close() })
	got, err := NewRepo(st2.DB()).Get(ctx, created.ID)
	if err != nil || got.UserAgent != "Arrmada/1.0 (+test)" {
		t.Fatalf("after restart: %+v, %v", got, err)
	}
}

// A blank User-Agent is stored as NULL and read back empty; Update can set and clear it.
func TestUserAgentUpdateAndClear(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t)
	created, _ := repo.Create(ctx, Indexer{Name: "A", Kind: KindNewznab, URL: "http://x/api", Enabled: true})
	var raw *string
	if err := repo.db.QueryRowContext(ctx, `SELECT user_agent FROM indexers WHERE id=?`, created.ID).Scan(&raw); err != nil || raw != nil {
		t.Fatalf("unset should be NULL, got %v, %v", raw, err)
	}
	created.UserAgent = "  Mine/2  "
	if err := repo.Update(ctx, created); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.Get(ctx, created.ID); got.UserAgent != "Mine/2" {
		t.Fatalf("UserAgent = %q, want trimmed Mine/2", got.UserAgent)
	}
	created.UserAgent = ""
	if err := repo.Update(ctx, created); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.Get(ctx, created.ID); got.UserAgent != "" {
		t.Fatalf("UserAgent = %q after clear", got.UserAgent)
	}
}

// Caps, search and the NZB fetch all send the stored User-Agent; an update changes what the
// next search sends; an indexer with none sends Go's default.
func TestUserAgentSentOnCapsSearchAndNZBFetch(t *testing.T) {
	ctx := context.Background()
	rec, srv := newUARecorder(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := NewService(st.DB(), slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	s.SetUsenetAvailable(func() bool { return true })
	idx, err := s.Create(ctx, Indexer{Name: "Geek", Kind: KindNewznab, URL: srv.URL + "/api", APIKey: "k", UserAgent: "Arrmada-Test/1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Test(ctx, idx.ID); err != nil {
		t.Fatalf("test: %v", err)
	}
	if got := rec.last("/api"); got != "Arrmada-Test/1" {
		t.Errorf("caps sent User-Agent %q", got)
	}

	if _, err := s.Search(ctx, SearchQuery{Text: "Some Movie", MediaType: MediaMovie, Limit: 10}); err != nil {
		t.Fatalf("search: %v", err)
	}
	if got := rec.last("/api"); got != "Arrmada-Test/1" {
		t.Errorf("search sent User-Agent %q", got)
	}

	if _, err := s.Fetch(ctx, "Geek", srv.URL+"/getnzb?r=abcdefghijk"); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := rec.last("/getnzb"); got != "Arrmada-Test/1" {
		t.Errorf("NZB fetch sent User-Agent %q", got)
	}

	idx.UserAgent = "Changed/2"
	if err := s.Update(ctx, idx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Search(ctx, SearchQuery{Text: "Some Movie", MediaType: MediaMovie, Limit: 10}); err != nil {
		t.Fatalf("search: %v", err)
	}
	if got := rec.last("/api"); got != "Changed/2" {
		t.Errorf("search after update sent User-Agent %q", got)
	}

	idx.UserAgent = ""
	if err := s.Update(ctx, idx); err != nil {
		t.Fatal(err)
	}
	_, _ = s.Search(ctx, SearchQuery{Text: "Some Movie", MediaType: MediaMovie, Limit: 10})
	if got := rec.last("/api"); got != "Go-http-client/1.1" {
		t.Errorf("unset indexer sent User-Agent %q, want the default", got)
	}
	if _, err := s.Fetch(ctx, "Geek", srv.URL+"/getnzb?r=abcdefghijk"); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := rec.last("/getnzb"); got != "Go-http-client/1.1" {
		t.Errorf("unset NZB fetch sent User-Agent %q, want the default", got)
	}
}
