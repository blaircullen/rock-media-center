package applog

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRedactPath(t *testing.T) {
	cases := map[string]string{
		"/api/items/m12":        "/api/items/{id}",
		"/api/items/m12v3/play": "/api/items/{id}/play",
		"/api/session/2f1c7e0a-3b4d-4c5e-8f9a-0b1c2d3e4f5a/sync": "/api/session/{id}/sync",
		"/api/users/u5":                        "/api/users/{id}",
		"/api/v1/movies/12/releases":           "/api/v1/movies/{id}/releases",
		"/api/items/m12/file/1234567/download": "/api/items/{id}/file/{id}/download",
		"/api/libraries/arrmada-movies":        "/api/libraries/arrmada-movies",
		"/api/me/items-in-progress":            "/api/me/items-in-progress",
		"/":                                    "/",
		"":                                     "",
	}
	for in, want := range cases {
		if got := RedactPath(in); got != want {
			t.Errorf("RedactPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRouteLabel(t *testing.T) {
	mux := http.NewServeMux()
	var label string
	mux.HandleFunc("POST /api/items/{id}/play", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("/", func(http.ResponseWriter, *http.Request) {})

	r := httptest.NewRequest("POST", "/api/items/b45/play", nil)
	mux.ServeHTTP(httptest.NewRecorder(), r)
	if label = RouteLabel(r); label != "POST /api/items/{id}/play" {
		t.Errorf("matched route labelled %q", label)
	}
	// The catch-all says nothing; the redacted path does.
	r = httptest.NewRequest("GET", "/api/items/b45/nope", nil)
	mux.ServeHTTP(httptest.NewRecorder(), r)
	if label = RouteLabel(r); label != "GET /api/items/{id}/nope" {
		t.Errorf("unmatched route labelled %q", label)
	}
}
