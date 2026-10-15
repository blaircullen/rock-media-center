package requests

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	"github.com/tristenlammi/arrmada/internal/movies"
	"github.com/tristenlammi/arrmada/internal/series"
)

// The Activity queue's requester chips: every open request (waiting or in progress) maps
// to the library item it matches — movies and shows by TMDB id — with the owner first and
// each follower once. Delivered, declined and
// not-yet-in-the-library requests name nobody.
func TestRequestersByLibrary(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	db := s.repo.db
	log := slog.Default()
	s.movies = newFakeMovies(movies.Movie{TMDBID: 100, Title: "Heat"}, movies.Movie{TMDBID: 101, Title: "Up"}, movies.Movie{TMDBID: 102, Title: "Jaws"})
	s.series = series.NewService(db, nil, t.TempDir(), log)
	for _, q := range []string{
		`INSERT INTO series (id, tmdb_id, title, monitored) VALUES (5, 200, 'Harbour Lights', 1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	mkReq := func(rq Request) int64 {
		t.Helper()
		got, err := s.repo.Create(ctx, rq)
		if err != nil {
			t.Fatal(err)
		}
		return got.ID
	}
	heat := mkReq(Request{MediaType: "movie", TMDBID: 100, Title: "Heat", Status: StatusPending, RequestedBy: 1, RequestedByName: "mum"})
	if err := s.repo.AddSubscriber(ctx, heat, 2, "dad"); err != nil {
		t.Fatal(err)
	}
	if err := s.repo.AddSubscriber(ctx, heat, 3, "mum"); err != nil { // a second account with the same name: named once
		t.Fatal(err)
	}
	mkReq(Request{MediaType: "series", TMDBID: 200, Title: "Harbour Lights", Status: StatusApproved, RequestedBy: 2, RequestedByName: "dad"})
	// Not waited on: delivered, declined, or not in the library.
	up := mkReq(Request{MediaType: "movie", TMDBID: 101, Title: "Up", Status: StatusApproved, RequestedBy: 6, RequestedByName: "done"})
	if err := s.repo.MarkReady(ctx, up, 100); err != nil {
		t.Fatal(err)
	}
	mkReq(Request{MediaType: "movie", TMDBID: 102, Title: "Jaws", Status: StatusDeclined, RequestedBy: 7, RequestedByName: "no"})
	mkReq(Request{MediaType: "movie", TMDBID: 999, Title: "Elsewhere", Status: StatusPending, RequestedBy: 8, RequestedByName: "nowhere"})

	got, err := s.RequestersByLibrary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"movie:1":  "[mum dad]",
		"series:5": "[dad]",
	}
	if len(got) != len(want) {
		t.Errorf("got %v, want %v", got, want)
	}
	for k, w := range want {
		if fmt.Sprint(got[k]) != w {
			t.Errorf("%s: %v, want %s", k, got[k], w)
		}
	}
}
