package plex

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakePlaylists is a small in-memory Plex playlist API.
type fakePlaylists struct {
	mu    sync.Mutex
	items []map[string]string // playlist "100": ratingKey + playlistItemID
	next  int
	log   []string
}

func (f *fakePlaylists) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.log = append(f.log, r.Method+" "+r.URL.Path)
		addURI := func() {
			uri := r.URL.Query().Get("uri")
			const pre = "server://MID123/com.plexapp.plugins.library/library/metadata/"
			if !strings.HasPrefix(uri, pre) {
				t.Errorf("bad uri %q", uri)
				return
			}
			for _, k := range strings.Split(strings.TrimPrefix(uri, pre), ",") {
				f.next++
				f.items = append(f.items, map[string]string{"ratingKey": k, "playlistItemID": strconv.Itoa(f.next)})
			}
		}
		switch {
		case r.URL.Path == "/identity":
			_, _ = w.Write([]byte(`{"MediaContainer":{"machineIdentifier":"MID123","version":"1"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/playlists":
			if r.URL.Query().Get("playlistType") != "audio" {
				http.Error(w, "missing playlistType", 400)
				return
			}
			_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[
			 {"ratingKey":"7","title":"All Music","smart":true},
			 {"ratingKey":"8","title":"Recent","smart":"1"},
			 {"ratingKey":"100","title":"Spotify Mirror","smart":false}]}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/playlists":
			q := r.URL.Query()
			if q.Get("type") != "audio" || q.Get("smart") != "0" || q.Get("title") != "Spotify Mirror" {
				http.Error(w, "bad create params", 400)
				return
			}
			addURI()
			_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"100","title":"Spotify Mirror"}]}}`))
		case r.Method == http.MethodPut && r.URL.Path == "/playlists/100/items":
			addURI()
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/playlists/100/items/"):
			id := strings.TrimPrefix(r.URL.Path, "/playlists/100/items/")
			for i, it := range f.items {
				if it["playlistItemID"] == id {
					f.items = append(f.items[:i], f.items[i+1:]...)
					return
				}
			}
			http.NotFound(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/playlists/100/items":
			var md []map[string]string
			for _, it := range f.items {
				md = append(md, map[string]string{"ratingKey": it["ratingKey"], "playlistItemID": it["playlistItemID"], "title": "T" + it["ratingKey"], "grandparentTitle": "A", "parentTitle": "B"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"MediaContainer": map[string]any{"Metadata": md}})
		default:
			http.NotFound(w, r)
		}
	})
}

func TestPlaylistsLeavesOutSmart(t *testing.T) {
	srv := httptest.NewServer((&fakePlaylists{}).handler(t))
	defer srv.Close()
	pls, err := New(srv.URL, "tok").Playlists(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pls) != 1 || pls[0].RatingKey != "100" || pls[0].Title != "Spotify Mirror" {
		t.Fatalf("playlists = %+v", pls)
	}
}

func TestPlaylistCreateAddReadRemove(t *testing.T) {
	f := &fakePlaylists{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	c := New(srv.URL, "tok")
	ctx := context.Background()

	key, err := c.CreatePlaylist(ctx, "Spotify Mirror", []string{"11"})
	if err != nil || key != "100" {
		t.Fatalf("create = %q, %v", key, err)
	}
	if err := c.AddToPlaylist(ctx, key, []string{"22", "33"}); err != nil {
		t.Fatal(err)
	}
	items, err := c.PlaylistItems(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || items[1].RatingKey != "22" || items[1].Title != "T22" || items[1].Artist != "A" || items[1].PlaylistItemID == "" {
		t.Fatalf("items = %+v", items)
	}
	if err := c.RemoveFromPlaylist(ctx, key, items[1].PlaylistItemID); err != nil {
		t.Fatal(err)
	}
	items, err = c.PlaylistItems(ctx, key)
	if err != nil || len(items) != 2 || items[0].RatingKey != "11" || items[1].RatingKey != "33" {
		t.Fatalf("after remove = %+v, %v", items, err)
	}
}

func TestPlaylistWritesRejectEmptyInput(t *testing.T) {
	srv := httptest.NewServer((&fakePlaylists{}).handler(t))
	defer srv.Close()
	c := New(srv.URL, "tok")
	if _, err := c.CreatePlaylist(context.Background(), "x", nil); err == nil {
		t.Fatal("create with no tracks accepted")
	}
	if err := c.AddToPlaylist(context.Background(), "100", nil); err == nil {
		t.Fatal("add of no tracks accepted")
	}
}

func TestPlaylistsUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	pls, err := New(srv.URL, "bad").Playlists(context.Background())
	if !errors.Is(err, ErrUnauthorized) || pls != nil {
		t.Fatalf("got %v, %v", pls, err)
	}
}
