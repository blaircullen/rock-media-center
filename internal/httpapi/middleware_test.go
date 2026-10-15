package httpapi

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

// The request log names the route, not the path: /api/v1/movies/12/releases says which
// title was opened.
func TestRequestLogUsesRoutePattern(t *testing.T) {
	var buf bytes.Buffer
	srv := New(Deps{Log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))})

	// The discover row has no digit to redact, so only the matched pattern turns
	// "trending" into {kind}: it proves r.Pattern reaches the logger.
	for _, path := range []string{"/api/v1/movies/12/releases", "/api/v1/discover/rows/trending", "/api/v1/no/such/thing/34"} {
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr = "192.168.1.20:5000"
		srv.Handler.ServeHTTP(httptest.NewRecorder(), r) // anonymous: answered by the scope check
	}

	logged := buf.String()
	if !strings.Contains(logged, "GET /api/v1/movies/{id}/releases") {
		t.Errorf("no route pattern in the log:\n%s", logged)
	}
	if !strings.Contains(logged, "GET /api/v1/discover/rows/{kind}") {
		t.Errorf("the matched pattern didn't reach the request log:\n%s", logged)
	}
	if !strings.Contains(logged, "GET /api/v1/no/such/thing/{id}") {
		t.Errorf("an unmatched path wasn't logged redacted:\n%s", logged)
	}
	if strings.Contains(logged, "/12/") || strings.Contains(logged, "/34") {
		t.Errorf("the log carries an id from the path:\n%s", logged)
	}
	if !strings.Contains(logged, "status=401") {
		t.Errorf("status missing:\n%s", logged)
	}
}
