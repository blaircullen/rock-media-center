package musicengine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func vpnServer(t *testing.T, status int, body string, calls *atomic.Int32, gotKey *atomic.Value) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if gotKey != nil {
			gotKey.Store(r.Header.Get("X-API-Key"))
		}
		if r.URL.Path != "/v1/vpn/status" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func vpnCheck(srv *httptest.Server, clock *fakeClock) *GluetunVPN {
	return NewGluetunVPN(GluetunVPNConfig{URL: func() string { return srv.URL }, Now: clock.Now})
}

func TestVPNRunningIsOK(t *testing.T) {
	var calls atomic.Int32
	srv := vpnServer(t, 200, `{"status":"running"}`, &calls, nil)
	ok, why := vpnCheck(srv, &fakeClock{t: time.Now()}).Check(context.Background())
	if !ok || why != "" {
		t.Fatalf("Check = %v, %q, want OK", ok, why)
	}
}

func TestVPNNotOKStates(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"stopped", 200, `{"status":"stopped"}`, `"stopped"`},
		{"crashed", 200, `{"status":"crashed"}`, `"crashed"`},
		{"empty status", 200, `{}`, "not running"},
		{"server error", 500, `boom`, "HTTP 500"},
		{"unauthorised", 401, `{"error":"unauthorized"}`, "HTTP 401"},
		{"not json", 200, `<html>`, "unreadable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := vpnServer(t, c.status, c.body, &calls, nil)
			ok, why := vpnCheck(srv, &fakeClock{t: time.Now()}).Check(context.Background())
			if ok || !strings.Contains(why, c.want) {
				t.Fatalf("Check = %v, %q, want not OK mentioning %q", ok, why, c.want)
			}
		})
	}
}

func TestVPNUnreachableIsNotOK(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	g := NewGluetunVPN(GluetunVPNConfig{URL: func() string { return url }})
	ok, why := g.Check(context.Background())
	if ok || !strings.Contains(why, "Couldn't reach") {
		t.Fatalf("Check = %v, %q, want unreachable", ok, why)
	}
}

// A control server that never answers is cut off at the deadline, and that is not OK.
func TestVPNHardDeadline(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	g := NewGluetunVPN(GluetunVPNConfig{URL: func() string { return srv.URL }, Timeout: 100 * time.Millisecond})
	start := time.Now()
	ok, why := g.Check(context.Background())
	if ok || !strings.Contains(why, "Couldn't reach") {
		t.Fatalf("Check = %v, %q", ok, why)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Check took %s, the deadline did not cut it off", d)
	}
}

// Answers are reused for a minute and asked again after.
func TestVPNCachesForAMinute(t *testing.T) {
	var calls atomic.Int32
	srv := vpnServer(t, 200, `{"status":"running"}`, &calls, nil)
	clock := &fakeClock{t: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	g := vpnCheck(srv, clock)
	for range 3 {
		g.Check(context.Background())
	}
	if calls.Load() != 1 {
		t.Fatalf("server asked %d times within the minute, want 1", calls.Load())
	}
	clock.Advance(59 * time.Second)
	g.Check(context.Background())
	if calls.Load() != 1 {
		t.Errorf("asked again at 59 s (%d calls)", calls.Load())
	}
	clock.Advance(2 * time.Second)
	g.Check(context.Background())
	if calls.Load() != 2 {
		t.Errorf("not asked again after 61 s (%d calls)", calls.Load())
	}
}

// A failure is cached too, and a changed URL is not answered from the old URL's cache.
func TestVPNCacheIsPerURL(t *testing.T) {
	var up, down atomic.Int32
	srvUp := vpnServer(t, 200, `{"status":"running"}`, &up, nil)
	srvDown := vpnServer(t, 200, `{"status":"stopped"}`, &down, nil)
	cur := srvDown.URL
	g := NewGluetunVPN(GluetunVPNConfig{URL: func() string { return cur }, Now: (&fakeClock{t: time.Now()}).Now})
	if ok, _ := g.Check(context.Background()); ok {
		t.Fatal("stopped tunnel reported OK")
	}
	g.Check(context.Background())
	if down.Load() != 1 {
		t.Errorf("failure not cached: %d calls", down.Load())
	}
	cur = srvUp.URL
	if ok, why := g.Check(context.Background()); !ok {
		t.Fatalf("after the URL changed Check = %v, %q", ok, why)
	}
}

func TestVPNSendsTheAPIKey(t *testing.T) {
	var calls atomic.Int32
	var got atomic.Value
	srv := vpnServer(t, 200, `{"status":"running"}`, &calls, &got)
	g := NewGluetunVPN(GluetunVPNConfig{URL: func() string { return srv.URL }, APIKey: func() string { return " s3cret\n" }})
	if ok, why := g.Check(context.Background()); !ok {
		t.Fatalf("Check = %v, %q", ok, why)
	}
	if got.Load() != "s3cret" {
		t.Errorf("X-API-Key = %v, want s3cret", got.Load())
	}
}

// A caller that gives up is not evidence about the tunnel: nothing is cached.
func TestVPNCallerCancelIsNotCached(t *testing.T) {
	var calls atomic.Int32
	srv := vpnServer(t, 200, `{"status":"running"}`, &calls, nil)
	g := vpnCheck(srv, &fakeClock{t: time.Now()})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ok, _ := g.Check(ctx); ok {
		t.Fatal("cancelled check reported OK")
	}
	if ok, why := g.Check(context.Background()); !ok {
		t.Fatalf("next check = %v, %q, want OK (the cancelled one must not be remembered)", ok, why)
	}
}

func TestVPNDefaultURL(t *testing.T) {
	g := NewGluetunVPN(GluetunVPNConfig{})
	if DefaultGluetunControlURL != "http://gluetun:8000" || g == nil {
		t.Fatal("default control URL changed")
	}
}
