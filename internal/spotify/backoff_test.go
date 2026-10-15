package spotify

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"
)

// kvStore satisfies NotBeforeStore.
type kvStore struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *kvStore) Get(_ context.Context, key, def string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.m[key]; ok {
		return v
	}
	return def
}

func (s *kvStore) Set(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = value
	return nil
}

// Astra #22: a 429's Retry-After is saved, and while it is in the future no token or API
// request leaves the process, including from a new client (a restart).
func TestRetryAfterIsPersistedAndBlocksRequestsAcrossRestart(t *testing.T) {
	f := newFake(t)
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "600")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}
	store := &kvStore{m: map[string]string{}}
	now := time.Unix(1_000_000, 0)
	mk := func() *Client {
		return f.client(t, func(c *Config) { c.NotBefore = store; c.Now = func() time.Time { return now } })
	}
	var out map[string]any
	err := mk().getJSON(t.Context(), "x", f.srv.URL+"/v1/ping", &out)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 429 {
		t.Fatalf("err = %v, want the 429", err)
	}
	if got, want := store.m[NotBeforeKey], strconv.FormatInt(now.Add(600*time.Second).Unix(), 10); got != want {
		t.Fatalf("saved not_before = %q, want %q", got, want)
	}

	// "Restart": a fresh client with the same store makes no token and no API call.
	tokens, apis := f.tokenCalls, f.apiCalls.Load()
	now = now.Add(599 * time.Second)
	err = mk().getJSON(t.Context(), "x", f.srv.URL+"/v1/ping", &out)
	var be *BackoffError
	if !errors.As(err, &be) {
		t.Fatalf("err = %v, want a BackoffError", err)
	}
	if f.tokenCalls != tokens || f.apiCalls.Load() != apis {
		t.Errorf("requests were sent during the cool-down: tokens %d->%d api %d->%d", tokens, f.tokenCalls, apis, f.apiCalls.Load())
	}

	// After the time passes, requests go out again.
	now = now.Add(2 * time.Second)
	f.handler = func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) }
	if err := mk().getJSON(t.Context(), "x", f.srv.URL+"/v1/ping", &out); err != nil {
		t.Fatalf("after the cool-down: %v", err)
	}
}
