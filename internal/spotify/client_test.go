package spotify

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSpotify serves both the accounts and API endpoints from one httptest server.
type fakeSpotify struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	tokenCalls int
	tokenSeq   int
	expiresIn  int
	// reject401 makes the API answer 401 to the first n API calls (any path).
	reject401 int
	// handler serves /v1/ paths; it is called after the auth check.
	handler  func(w http.ResponseWriter, r *http.Request)
	apiCalls atomic.Int32
}

func newFake(t *testing.T) *fakeSpotify {
	f := &fakeSpotify{t: t, expiresIn: 3600}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSpotify) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/token" {
		f.mu.Lock()
		f.tokenCalls++
		f.tokenSeq++
		seq, exp := f.tokenSeq, f.expiresIn
		f.mu.Unlock()
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("cid:csecret"))
		if r.Header.Get("Authorization") != want || r.FormValue("grant_type") != "client_credentials" || r.Method != http.MethodPost {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok` + string(rune('0'+seq)) + `","token_type":"Bearer","expires_in":` + itoa(exp) + `}`))
		return
	}
	f.apiCalls.Add(1)
	f.mu.Lock()
	rej := f.reject401 > 0
	if rej {
		f.reject401--
	}
	f.mu.Unlock()
	if rej {
		http.Error(w, `{"error":{"status":401,"message":"The access token expired"}}`, http.StatusUnauthorized)
		return
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer tok") {
		http.Error(w, "no bearer", http.StatusUnauthorized)
		return
	}
	f.handler(w, r)
}

func itoa(n int) string { return strconv.Itoa(n) }

func (f *fakeSpotify) client(t *testing.T, mut func(*Config)) *Client {
	cfg := Config{ClientID: "cid", ClientSecret: "csecret", APIBase: f.srv.URL, AccountsBase: f.srv.URL}
	if mut != nil {
		mut(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewRequiresCredentials(t *testing.T) {
	if _, err := New(Config{ClientID: "x"}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

func TestTokenCachedUntilExpiryThenRefreshed(t *testing.T) {
	f := newFake(t)
	f.expiresIn = 3600
	f.handler = func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) }
	now := time.Unix(1_000_000, 0)
	c := f.client(t, func(cfg *Config) { cfg.Now = func() time.Time { return now } })

	var out map[string]any
	for i := 0; i < 3; i++ {
		if err := c.getJSON(t.Context(), "x", f.srv.URL+"/v1/ping", &out); err != nil {
			t.Fatal(err)
		}
	}
	if f.tokenCalls != 1 {
		t.Fatalf("token calls = %d, want 1 (cached)", f.tokenCalls)
	}
	now = now.Add(3600*time.Second - tokenSkew + time.Second) // inside the skew window
	if err := c.getJSON(t.Context(), "x", f.srv.URL+"/v1/ping", &out); err != nil {
		t.Fatal(err)
	}
	if f.tokenCalls != 2 {
		t.Fatalf("token calls = %d after expiry, want 2", f.tokenCalls)
	}
}

// An expired token (401 from the API) is replaced once and the call retried.
func TestExpiredTokenRefreshedOnceAndRetried(t *testing.T) {
	f := newFake(t)
	f.handler = func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) }
	c := f.client(t, nil)
	var out map[string]any
	if err := c.getJSON(t.Context(), "x", f.srv.URL+"/v1/ping", &out); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.reject401 = 1
	f.mu.Unlock()
	if err := c.getJSON(t.Context(), "x", f.srv.URL+"/v1/ping", &out); err != nil {
		t.Fatalf("call after 401: %v", err)
	}
	if f.tokenCalls != 2 {
		t.Fatalf("token calls = %d, want 2 (initial + one refresh)", f.tokenCalls)
	}
	if out["ok"] != true {
		t.Fatalf("retry body not decoded: %v", out)
	}
}

// A second 401 is an error with the status and body, not a refresh loop.
func TestSecond401IsReported(t *testing.T) {
	f := newFake(t)
	f.reject401 = 99
	f.handler = func(w http.ResponseWriter, r *http.Request) {}
	c := f.client(t, nil)
	var out map[string]any
	err := c.getJSON(t.Context(), "GET /v1/ping", f.srv.URL+"/v1/ping", &out)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 401 || !strings.Contains(ae.Body, "expired") {
		t.Fatalf("err = %v, want APIError 401 with body", err)
	}
	if f.tokenCalls != 2 || f.apiCalls.Load() != 2 {
		t.Fatalf("token calls %d, api calls %d; want exactly one refresh and one retry", f.tokenCalls, f.apiCalls.Load())
	}
}

func TestTokenEndpointFailureKeepsStatusAndBody(t *testing.T) {
	f := newFake(t)
	f.handler = func(w http.ResponseWriter, r *http.Request) {}
	c, _ := New(Config{ClientID: "cid", ClientSecret: "WRONG", APIBase: f.srv.URL, AccountsBase: f.srv.URL})
	var out map[string]any
	err := c.getJSON(t.Context(), "x", f.srv.URL+"/v1/ping", &out)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 400 || !strings.Contains(ae.Body, "invalid_client") {
		t.Fatalf("err = %v, want APIError 400 invalid_client", err)
	}
}

func TestRateLimitCarriesRetryAfter(t *testing.T) {
	f := newFake(t)
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}
	c := f.client(t, nil)
	var out map[string]any
	err := c.getJSON(t.Context(), "x", f.srv.URL+"/v1/ping", &out)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 429 || ae.RetryAfter != 7*time.Second {
		t.Fatalf("err = %v, want 429 with RetryAfter 7s", err)
	}
}
