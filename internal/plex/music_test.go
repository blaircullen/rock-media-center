package plex

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// fakeTracks serves n tracks from /library/sections/5/all in pages, honouring the
// X-Plex-Container-* params and capping pages at 2 below what's asked to prove paging.
func fakeTracksServer(t *testing.T, n int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/library/sections/5/all" || r.URL.Query().Get("type") != "10" {
			http.NotFound(w, r)
			return
		}
		start, _ := strconv.Atoi(r.URL.Query().Get("X-Plex-Container-Start"))
		size := 2 // the server caps pages below the 500 asked for
		var items []string
		for i := start; i < start+size && i < n; i++ {
			items = append(items, fmt.Sprintf(`{"ratingKey":"%d","title":"T%d","grandparentTitle":"Artist","parentTitle":"Album","userRating":8.0,"viewCount":%d,"Media":[{"Part":[{"file":"/music/Artist/Album/%02d. T%d.flac"}]}]}`, i, i, i, i, i))
		}
		fmt.Fprintf(w, `{"MediaContainer":{"size":%d,"totalSize":%d,"Metadata":[%s]}}`, len(items), n, strings.Join(items, ","))
	}))
}

func TestTracksPagesAndFilePaths(t *testing.T) {
	srv := fakeTracksServer(t, 5)
	defer srv.Close()
	tracks, err := New(srv.URL, "tok").Tracks(context.Background(), "5")
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 5 {
		t.Fatalf("got %d tracks, want 5", len(tracks))
	}
	last := tracks[4]
	if last.RatingKey != "4" || last.Artist != "Artist" || last.Album != "Album" || last.UserRating != 8 || last.ViewCount != 4 {
		t.Fatalf("last track = %+v", last)
	}
	if len(last.Files) != 1 || last.Files[0] != "/music/Artist/Album/04. T4.flac" {
		t.Fatalf("files = %v", last.Files)
	}
}

func TestTrackIndexedFalseThenTrue(t *testing.T) {
	indexed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/library/sections/5/search" || r.URL.Query().Get("title") != "Song One" {
			http.NotFound(w, r)
			return
		}
		if !indexed {
			_, _ = w.Write([]byte(`{"MediaContainer":{"size":0}}`))
			return
		}
		_, _ = w.Write([]byte(`{"MediaContainer":{"size":2,"Metadata":[
		 {"ratingKey":"1","title":"Song One","grandparentTitle":"Other Artist","parentTitle":"Rec","Media":[{"Part":[{"file":"/m/Other/Rec/01.flac"}]}]},
		 {"ratingKey":"2","title":"Song One","grandparentTitle":"Artist","parentTitle":"Rec","Media":[{"Part":[{"file":"/m/Artist/Rec/01. Song One.flac"}]}]}]}}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")
	ctx := context.Background()
	const p = "/m/Artist/Rec/01. Song One.flac"
	got, err := c.TrackIndexed(ctx, "5", "Artist", "Rec", "Song One", p)
	if err != nil || got {
		t.Fatalf("before indexing: got %v, %v; want false, nil", got, err)
	}
	indexed = true
	got, err = c.TrackIndexed(ctx, "5", "artist", "rec", "Song One", p)
	if err != nil || !got {
		t.Fatalf("after indexing: got %v, %v; want true, nil", got, err)
	}
	// Same path under the wrong album is not a match.
	if got, _ := c.TrackIndexed(ctx, "5", "Artist", "Wrong Album", "Song One", p); got {
		t.Fatal("matched under the wrong album")
	}
}

func TestSetRatingRequestShape(t *testing.T) {
	var method, key, ident, rating, tok string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, tok = r.Method, r.Header.Get("X-Plex-Token")
		if r.URL.Path != "/:/rate" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		key, ident, rating = q.Get("key"), q.Get("identifier"), q.Get("rating")
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")
	if err := c.SetRating(context.Background(), "42", 8); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPut || key != "42" || ident != "com.plexapp.plugins.library" || rating != "8" || tok != "tok" {
		t.Fatalf("got %s key=%s identifier=%s rating=%s token=%s", method, key, ident, rating, tok)
	}
	if err := c.SetRating(context.Background(), "42", 11); err == nil {
		t.Fatal("rating 11 accepted")
	}
}

func TestUnauthorizedIsAnErrorNamingPlex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := New(srv.URL, "bad")
	ctx := context.Background()
	tracks, err := c.Tracks(ctx, "5")
	if !errors.Is(err, ErrUnauthorized) || tracks != nil || !strings.Contains(err.Error(), "plex") {
		t.Fatalf("Tracks: %v, %v", tracks, err)
	}
	if err := c.SetRating(ctx, "1", 4); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("SetRating: %v", err)
	}
}

func TestWriteFailureKeepsStatusAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad rating identifier", http.StatusBadRequest)
	}))
	defer srv.Close()
	err := New(srv.URL, "tok").SetRating(context.Background(), "1", 4)
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "bad rating identifier") {
		t.Fatalf("error lost status/body: %v", err)
	}
}
