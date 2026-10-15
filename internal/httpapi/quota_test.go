package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tristenlammi/arrmada/internal/auth"
	"github.com/tristenlammi/arrmada/internal/requests"
)

// quotaServer is requestsServer with request limits wired as main wires them: a person's
// own limits, else the global settings.
func quotaServer(t *testing.T) *routeServer {
	t.Helper()
	s := requestsServer(t)
	s.deps.Requests.SetQuotaLimits(func(ctx context.Context, uid int64) (requests.Limits, error) {
		own, err := s.auth.Quota(ctx, uid)
		if err != nil {
			return requests.Limits{}, err
		}
		global := requests.GlobalLimits(func(key string) string { return s.deps.Settings.Get(ctx, key, "") })
		return requests.LimitsFrom(global, own.Movies, own.Seasons), nil
	})
	return s
}

// Over the limit answers 429 with what was used and when it frees up; /me/quota says how
// much is left; staff aren't limited; a person's own limit overrides the global one.
func TestCreateReturns429(t *testing.T) {
	s := quotaServer(t)
	_, admin := s.user(t, "admin@example.com", auth.RoleAdmin)
	kid, cookie := s.user(t, "kid@example.com", auth.RoleRequester)
	if rec := s.doJSON("PUT", "/api/v1/settings", admin, `{"request_quota_days":7,"request_quota_movies":1}`); rec.Code != http.StatusOK {
		t.Fatalf("settings: HTTP %d %s", rec.Code, rec.Body)
	}
	movie := func(c *http.Cookie, id int) *httptest.ResponseRecorder {
		return s.doJSON("POST", "/api/v1/requests", c, fmt.Sprintf(`{"media_type":"movie","tmdb_id":%d,"title":"Movie %d"}`, id, id))
	}
	if rec := movie(cookie, 101); rec.Code != http.StatusOK {
		t.Fatalf("first movie: HTTP %d %s", rec.Code, rec.Body)
	}
	rec := movie(cookie, 102)
	var over struct {
		Message  string `json:"message"`
		Kind     string `json:"kind"`
		Limit    int    `json:"limit"`
		Used     int    `json:"used"`
		ResetsAt string `json:"resets_at"`
	}
	if rec.Code != http.StatusTooManyRequests || json.Unmarshal(rec.Body.Bytes(), &over) != nil ||
		over.Kind != "movie" || over.Limit != 1 || over.Used != 1 || over.ResetsAt == "" ||
		!strings.HasPrefix(over.Message, "You've used your 1 movie request this week. The next one frees up ") {
		t.Fatalf("second movie: HTTP %d %s", rec.Code, rec.Body)
	}
	var mine requests.QuotaStatus
	if rec := s.do("GET", "/api/v1/me/quota", cookie); rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &mine) != nil ||
		mine.Movie.Limit != 1 || mine.Movie.Used != 1 || mine.Season.Limit != 0 || mine.Days != 7 {
		t.Errorf("/me/quota: HTTP %d %s", rec.Code, rec.Body)
	}
	// Staff are never limited.
	for _, id := range []int{103, 104} {
		if rec := movie(admin, id); rec.Code != http.StatusOK {
			t.Errorf("staff movie %d: HTTP %d %s", id, rec.Code, rec.Body)
		}
	}
	// Their own limit (0: unlimited) beats the global one.
	if rec := s.doJSON("PUT", fmt.Sprintf("/api/v1/users/%d", kid.ID), admin, `{"quota":{"movies":0,"seasons":-1}}`); rec.Code != http.StatusOK {
		t.Fatalf("user quota: HTTP %d %s", rec.Code, rec.Body)
	}
	if rec := movie(cookie, 102); rec.Code != http.StatusOK {
		t.Errorf("with their own unlimited movies: HTTP %d %s", rec.Code, rec.Body)
	}
	if rec := s.do("GET", "/api/v1/users", admin); !strings.Contains(rec.Body.String(), `"quota":{"movies":0,"seasons":-1}`) {
		t.Errorf("users list doesn't carry their limits: %s", rec.Body)
	}
	// Only an admin sets the global limits, and only sensible ones.
	_, manager := s.user(t, "manager@example.com", auth.RoleManager)
	if rec := s.doJSON("PUT", "/api/v1/settings", manager, `{"request_quota_movies":5}`); rec.Code != http.StatusForbidden {
		t.Errorf("manager changing limits: HTTP %d", rec.Code)
	}
	if rec := s.doJSON("PUT", "/api/v1/settings", admin, `{"request_quota_days":0}`); rec.Code != http.StatusBadRequest {
		t.Errorf("a 0-day window: HTTP %d", rec.Code)
	}
}
