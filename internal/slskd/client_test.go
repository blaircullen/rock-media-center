package slskd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(srv *httptest.Server) *Client {
	c := New(srv.URL, "test-key")
	c.PollInterval = 5 * time.Millisecond
	return c
}

func TestSearchFlattensFiles(t *testing.T) {
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-key" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v0/searches":
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			if b["searchText"] != "artist album" || b["id"] == "" || b["searchTimeout"].(float64) != 20000 {
				t.Errorf("bad payload %v", b)
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v0/searches/"):
			if r.URL.Query().Get("includeResponses") != "true" {
				t.Errorf("includeResponses missing")
			}
			state := "InProgress"
			if atomic.AddInt32(&polls, 1) >= 2 {
				state = "Completed, TimedOut"
			}
			_, _ = w.Write([]byte(`{"state":"` + state + `","responses":[{"username":"bob","uploadSpeed":1000,"queueLength":3,"hasFreeUploadSlot":true,
			"files":[{"filename":"a\\b\\01.flac","size":30000000,"extension":"flac","bitDepth":16,"sampleRate":44100,"length":200},
			{"filename":"a\\b\\02.flac","size":1,"isLocked":true}]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	got, err := newTestClient(srv).Search(context.Background(), "artist album", SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Username != "bob" || got[0].Size != 30000000 || got[0].BitDepth != 16 || !got[0].FreeSlot || got[0].QueueLength != 3 {
		t.Fatalf("got %+v", got)
	}
}

func TestSearchNothingIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"state":"Completed, ResponseLimitReached","responses":[]}`))
		}
	}))
	defer srv.Close()
	got, err := newTestClient(srv).Search(context.Background(), "nothing here", SearchOptions{})
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestFailedTransferReportsFailedNotComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"username":"bob","directories":[{"directory":"d","files":[
		{"id":"t1","filename":"x.flac","state":"Completed, Errored","size":10,"bytesTransferred":3,"exception":"remote closed"}]}]}`))
	}))
	defer srv.Close()
	c := newTestClient(srv)
	tr, err := c.GetDownload(context.Background(), "bob", "x.flac")
	if err != nil || tr.Phase != PhaseFailed {
		t.Fatalf("tr=%+v err=%v", tr, err)
	}
	if _, err := c.WaitDownload(context.Background(), "bob", "x.flac", time.Minute); err == nil {
		t.Fatal("WaitDownload returned nil for a failed transfer")
	}
}

func TestSucceededTransfer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"t1","filename":"x.flac","state":"Completed, Succeeded","size":10,"bytesTransferred":10}]`))
	}))
	defer srv.Close()
	tr, err := newTestClient(srv).WaitDownload(context.Background(), "bob", "x.flac", time.Minute)
	if err != nil || tr.Phase != PhaseSucceeded {
		t.Fatalf("tr=%+v err=%v", tr, err)
	}
}

func TestEnqueueSendsPayloadAndSurfacesFailed(t *testing.T) {
	var fail bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v0/transfers/downloads/bob" {
			t.Errorf("route %s %s", r.Method, r.URL.Path)
		}
		var b []map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		if len(b) != 1 || b[0]["filename"] != "x.flac" || b[0]["size"].(float64) != 10 {
			t.Errorf("payload %v", b)
		}
		if fail {
			_, _ = w.Write([]byte(`{"enqueued":[],"failed":["File not shared"]}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"enqueued":[{"id":"t1"}],"failed":[]}`))
	}))
	defer srv.Close()
	c := newTestClient(srv)
	if err := c.Enqueue(context.Background(), "bob", "x.flac", 10); err != nil {
		t.Fatal(err)
	}
	fail = true
	if err := c.Enqueue(context.Background(), "bob", "x.flac", 10); err == nil || !strings.Contains(err.Error(), "File not shared") {
		t.Fatalf("err=%v", err)
	}
}

func TestStalledTransferIsCancelled(t *testing.T) {
	var deleted int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/api/v0/transfers/downloads/bob/t1" {
			atomic.AddInt32(&deleted, 1)
			return
		}
		_, _ = w.Write([]byte(`[{"id":"t1","filename":"x.flac","state":"Queued, Remotely","size":10,"bytesTransferred":0}]`))
	}))
	defer srv.Close()
	_, err := newTestClient(srv).WaitDownload(context.Background(), "bob", "x.flac", 20*time.Millisecond)
	if err == nil || atomic.LoadInt32(&deleted) != 1 {
		t.Fatalf("err=%v deleted=%d", err, deleted)
	}
}

func TestConnectionRefusedIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := New(url, "k")
	_, err := c.Search(context.Background(), "q", SearchOptions{})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

func TestBadGatewayIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream down", http.StatusBadGateway)
	}))
	defer srv.Close()
	_, err := newTestClient(srv).Search(context.Background(), "q", SearchOptions{})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

func TestWrongKeyIsAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "right" {
			http.Error(w, "bad key", http.StatusUnauthorized)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "wrong")
	_, err := c.Search(context.Background(), "q", SearchOptions{})
	if !errors.Is(err, ErrAuth) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

func TestMissingTransferIsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"username":"bob","directories":[]}`))
	}))
	defer srv.Close()
	_, err := newTestClient(srv).GetDownload(context.Background(), "bob", "x.flac")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v", err)
	}
}

func TestOtherStatusKeepsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom detail", http.StatusInternalServerError)
	}))
	defer srv.Close()
	err := newTestClient(srv).Enqueue(context.Background(), "bob", "x.flac", 1)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 500 || !strings.Contains(ae.Body, "boom detail") {
		t.Fatalf("err=%v", err)
	}
}
