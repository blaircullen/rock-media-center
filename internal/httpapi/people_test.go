package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/tristenlammi/arrmada/internal/auth"
)

// peopleKeys is every key a /people row may carry. A new key has to be added here on
// purpose.
var peopleKeys = map[string]bool{
	"id": true, "username": true, "role": true, "disabled": true, "created_at": true,
	"auto_approve_movie": true, "auto_approve_series": true,
	"plex_linked": true, "plex_username": true, "plex_only": true, "plex_blocked": true, "quota": true,
	"jellyfin_linked": true, "jellyfin_username": true, "jellyfin_only": true, "jellyfin_blocked": true,
	"sessions": true, "web_last": true, "last_seen": true,
}

// Every row's keys are on the allowlist.
func TestPeopleListKeys(t *testing.T) {
	s := newRouteServer(t, nil)
	_, adminC := s.user(t, "owner", auth.RoleAdmin)
	s.user(t, "kid", auth.RoleRequester)

	rec := s.do("GET", "/api/v1/people", adminC)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d %s", rec.Code, rec.Body)
	}
	var out struct {
		People []map[string]json.RawMessage `json:"people"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.People) != 2 {
		t.Fatalf("people = %s (%v)", rec.Body, err)
	}
	for _, p := range out.People {
		for k := range p {
			if !peopleKeys[k] {
				t.Errorf("unexpected key %q in a people row", k)
			}
		}
	}
}

// Last seen is the web app's last use; sessions counts only live browser sessions.
func TestPeopleLastSeenFromSessions(t *testing.T) {
	s := newRouteServer(t, nil)
	ctx := context.Background()
	_, adminC := s.user(t, "owner", auth.RoleAdmin)
	kid, _ := s.user(t, "kid", auth.RoleRequester) // one session, from the helper's sign-in
	if _, _, err := s.auth.CreateSessionFrom(ctx, kid.ID, auth.SessionClient{UserAgent: "Firefox"}); err != nil {
		t.Fatal(err)
	}
	quiet, _ := s.user(t, "quiet", auth.RoleReadonly)
	if _, err := s.auth.RevokeUserSessions(ctx, quiet.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()

	var out struct {
		People []personRow `json:"people"`
	}
	rec := s.do("GET", "/api/v1/people", adminC)
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("HTTP %d %s", rec.Code, rec.Body)
	}
	byName := map[string]personRow{}
	for _, p := range out.People {
		byName[p.Username] = p
	}
	k := byName["kid"]
	if k.Sessions != 2 {
		t.Errorf("kid sessions = %d, want 2", k.Sessions)
	}
	if k.WebLast < now-60_000 {
		t.Errorf("kid web_last = %d, want about now (%d)", k.WebLast, now)
	}
	if k.LastSeen != k.WebLast {
		t.Errorf("kid last_seen = %d, want the web session's %d", k.LastSeen, k.WebLast)
	}
	q := byName["quiet"]
	if q.Sessions != 0 || q.LastSeen != 0 {
		t.Errorf("quiet = %+v, want never seen", q)
	}
}

// Only an admin may list people.
func TestPeopleAdminOnly(t *testing.T) {
	s := newRouteServer(t, nil)
	for _, role := range []auth.Role{auth.RoleManager, auth.RoleRequester, auth.RoleReadonly} {
		_, c := s.user(t, "u-"+string(role), role)
		if rec := s.do("GET", "/api/v1/people", c); rec.Code != http.StatusForbidden {
			t.Errorf("%s: HTTP %d, want 403", role, rec.Code)
		}
	}
}
