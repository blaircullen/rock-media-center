package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// /api/health answers with the commit and never a "busy" block, from loopback or anywhere
// else: nothing runs in the background that an update would have to wait for.
func TestHealthHasNoBusyBlock(t *testing.T) {
	s := newRouteServer(t, nil)
	h := New(s.deps).Handler
	for _, remote := range []string{"127.0.0.1:1234", "192.168.1.20:5000"} {
		r := httptest.NewRequest("GET", "http://arrmada.local/api/health", nil)
		r.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: HTTP %d", remote, rec.Code)
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: %v", remote, err)
		}
		if _, ok := body["busy"]; ok {
			t.Errorf("%s: busy block present: %s", remote, rec.Body)
		}
		if _, ok := body["commit"]; !ok {
			t.Errorf("%s: commit missing", remote)
		}
	}
}
