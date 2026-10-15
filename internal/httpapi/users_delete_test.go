package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/tristenlammi/arrmada/internal/auth"
)

// snapshotRecorder stands in for the database copy: it records each call and whether the
// user still existed at that moment (the copy must come before the delete).
type snapshotRecorder struct {
	calls     []string
	userAlive []bool
	fail      error
	check     func() bool
}

func (s *snapshotRecorder) fn(ctx context.Context, kind string) (string, error) {
	s.calls = append(s.calls, kind)
	if s.check != nil {
		s.userAlive = append(s.userAlive, s.check())
	}
	if s.fail != nil {
		return "", s.fail
	}
	return "/data/backups/arrmada-" + kind + "-20260101T000000Z.db", nil
}

func (s *routeServer) userExists(t *testing.T, id int64) bool {
	t.Helper()
	_, err := s.auth.UserByID(context.Background(), id)
	return err == nil
}

// A user can be deleted without typing anything, but the database is still copied first.
func TestDeleteUserTakesSnapshot(t *testing.T) {
	snap := &snapshotRecorder{}
	s := newRouteServer(t, func(d *Deps) { d.Snapshot = snap.fn })
	_, admin := s.user(t, "owner@example.com", auth.RoleAdmin)
	guest, _ := s.user(t, "guest@example.com", auth.RoleRequester)
	snap.check = func() bool { return s.userExists(t, guest.ID) }

	rec := s.do("DELETE", fmt.Sprintf("/api/v1/users/%d", guest.ID), admin)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body)
	}
	if len(snap.calls) != 1 || snap.calls[0] != "pre-delete-user" {
		t.Fatalf("snapshot calls = %v, want one pre-delete-user", snap.calls)
	}
	if !snap.userAlive[0] {
		t.Error("the snapshot ran after the user was already deleted")
	}
	if s.userExists(t, guest.ID) {
		t.Error("user should be deleted")
	}
}

// If the copy can't be taken, nothing is deleted.
func TestDeleteUserSnapshotFailureKeepsUser(t *testing.T) {
	snap := &snapshotRecorder{fail: errors.New("disk full")}
	s := newRouteServer(t, func(d *Deps) { d.Snapshot = snap.fn })
	_, admin := s.user(t, "owner@example.com", auth.RoleAdmin)
	guest, _ := s.user(t, "guest@example.com", auth.RoleRequester)

	rec := s.do("DELETE", fmt.Sprintf("/api/v1/users/%d", guest.ID), admin)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "nothing was deleted") {
		t.Fatalf("HTTP %d %s; want 500 saying nothing was deleted", rec.Code, rec.Body)
	}
	if !s.userExists(t, guest.ID) {
		t.Fatal("a failed safety copy must leave the user in place")
	}

	// No snapshot function wired at all is treated the same way.
	s2 := newRouteServer(t, nil)
	_, admin2 := s2.user(t, "owner@example.com", auth.RoleAdmin)
	guest2, _ := s2.user(t, "guest@example.com", auth.RoleRequester)
	if rec := s2.do("DELETE", fmt.Sprintf("/api/v1/users/%d", guest2.ID), admin2); rec.Code != http.StatusInternalServerError {
		t.Fatalf("no snapshot func: HTTP %d, want 500", rec.Code)
	}
	if !s2.userExists(t, guest2.ID) {
		t.Fatal("user deleted without a safety copy")
	}
}

// The impact endpoint is admin-only and carries counts.
func TestUserImpactRoute(t *testing.T) {
	s := newRouteServer(t, nil)
	_, admin := s.user(t, "owner@example.com", auth.RoleAdmin)
	_, mgr := s.user(t, "mgr@example.com", auth.RoleManager)
	kid, _ := s.user(t, "kid@example.com", auth.RoleRequester)
	if _, err := s.st.DB().Exec(`INSERT INTO requests (media_type, tmdb_id, title, requested_by, requested_by_name) VALUES ('movie', 1, 'A', ?, 'kid')`, kid.ID); err != nil {
		t.Fatal(err)
	}

	path := fmt.Sprintf("/api/v1/users/%d/impact", kid.ID)
	if rec := s.do("GET", path, mgr); rec.Code != http.StatusForbidden {
		t.Errorf("manager: HTTP %d, want 403", rec.Code)
	}
	rec := s.do("GET", path, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin: HTTP %d: %s", rec.Code, rec.Body)
	}
	var imp auth.UserImpact
	if err := json.Unmarshal(rec.Body.Bytes(), &imp); err != nil {
		t.Fatal(err)
	}
	if imp.Requests != 1 {
		t.Errorf("impact = %+v, want 1 request", imp)
	}
	if rec := s.do("GET", "/api/v1/users/9999/impact", admin); rec.Code != http.StatusNotFound {
		t.Errorf("unknown user: HTTP %d, want 404", rec.Code)
	}
}
