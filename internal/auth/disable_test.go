package auth

import (
	"context"
	"testing"
)

// Disabling signs someone out of the web app, and deletes nothing.
func TestDisableUserRevokesSessions(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	u, err := s.CreateUser(ctx, "kid@example.com", "supersecret", RoleRequester, false)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := s.CreateUser(ctx, "mum@example.com", "supersecret", RoleRequester, false)
	tok, _, _ := s.CreateSession(ctx, u.ID)
	otherTok, _, _ := s.CreateSession(ctx, other.ID)

	if err := s.SetDisabled(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ValidateSession(ctx, tok); err == nil {
		t.Error("a disabled user's session still validates")
	}
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ?`, u.ID).Scan(&n)
	if n != 0 {
		t.Errorf("%d sessions left after disable", n)
	}
	if _, err := s.ValidateSession(ctx, otherTok); err != nil {
		t.Errorf("someone else's session was dropped: %v", err)
	}
	if _, err := s.Authenticate(ctx, "kid@example.com", "supersecret"); err != ErrInvalidCredentials {
		t.Errorf("disabled user signed in: %v", err)
	}

	if err := s.SetDisabled(ctx, u.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "kid@example.com", "supersecret"); err != nil {
		t.Errorf("re-enabled user can't sign in: %v", err)
	}
	if err := s.SetDisabled(ctx, 9999, true); err != ErrNotFound {
		t.Errorf("unknown user: %v, want ErrNotFound", err)
	}
}
