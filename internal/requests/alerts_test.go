package requests

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/tristenlammi/arrmada/internal/eventbus"
	"github.com/tristenlammi/arrmada/internal/metadata"
	"github.com/tristenlammi/arrmada/internal/movies"
)

// An auto-approved request feeds the admin alert once, naming the title and requester;
// a pending request and a second person asking for the same title don't.
func TestCreatePublishesAutoApproved(t *testing.T) {
	f := newApproveFixture(t, metadata.SeriesDetails{})
	s, ctx := f.s, f.ctx
	s.bus = eventbus.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Already in the library, so approving takes the ErrExists path.
	if _, err := movies.NewRepo(s.repo.db).Create(ctx, movies.Movie{TMDBID: 949, Title: "Heat", Year: 1995}); err != nil {
		t.Fatal(err)
	}
	events, cancel := s.bus.Subscribe("request.auto_approved")
	defer cancel()
	next := func() map[string]any {
		select {
		case ev := <-events:
			return ev.Data.(map[string]any)
		case <-time.After(150 * time.Millisecond):
			return nil
		}
	}

	if _, _, err := s.Create(ctx, Request{MediaType: "movie", TMDBID: 949, Title: "Heat", Year: 1995, RequestedBy: 7, RequestedByName: "alice"}, CreateOptions{AutoApprove: true}); err != nil {
		t.Fatal(err)
	}
	got := next()
	if got == nil || got["title"] != "Heat" || got["requested_by"] != "alice" || got["year"] != 1995 {
		t.Fatalf("auto-approve event = %v", got)
	}
	if extra := next(); extra != nil {
		t.Fatalf("a second event: %v", extra)
	}
	// Someone else asking for it attaches as a subscriber: no new alert.
	if _, _, err := s.Create(ctx, Request{MediaType: "movie", TMDBID: 949, Title: "Heat", RequestedBy: 8, RequestedByName: "bob"}, CreateOptions{AutoApprove: true}); err != nil {
		t.Fatal(err)
	}
	// A request left pending isn't auto-approved.
	if _, _, err := s.Create(ctx, Request{MediaType: "movie", TMDBID: 950, Title: "Ronin", RequestedBy: 7, RequestedByName: "alice"}, CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// An import's old request (Silent) is approved without raising the alert.
	if _, err := movies.NewRepo(s.repo.db).Create(ctx, movies.Movie{TMDBID: 951, Title: "Collateral", Year: 2004}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(ctx, Request{MediaType: "movie", TMDBID: 951, Title: "Collateral", RequestedBy: 9, RequestedByName: "cara"},
		CreateOptions{AutoApprove: true, Silent: true, DeferSearch: true}); err != nil {
		t.Fatal(err)
	}
	if extra := next(); extra != nil {
		t.Fatalf("unexpected auto-approve event: %v", extra)
	}
}
