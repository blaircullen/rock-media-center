package auth

import (
	"context"
	"errors"
	"testing"
)

// The delete dialog's counts must be right.
func TestUserImpactCounts(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	u, err := s.CreateUser(ctx, "kid", "password123", RoleRequester, false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateUser(ctx, "parent", "password123", RoleAdmin, false)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO requests (media_type, tmdb_id, title, requested_by) VALUES ('movie', 1, 'A', ?), ('series', 2, 'B', ?), ('movie', 3, 'C', ?)`, u.ID, u.ID, other.ID)
	exec(`INSERT INTO push_subscriptions (user_id, endpoint, p256dh, auth) VALUES (?, 'https://push.example/1', 'k', 'a')`, u.ID)
	if _, _, err := s.CreateSession(ctx, u.ID); err != nil {
		t.Fatal(err)
	}

	imp, err := s.DeletionImpact(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := UserImpact{Requests: 2, Sessions: 1, PushSubscriptions: 1}
	if imp != want {
		t.Fatalf("impact = %+v\nwant     %+v", imp, want)
	}

	if _, err := s.DeletionImpact(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown user: err = %v, want ErrNotFound", err)
	}
}

// A brand-new account has nothing to lose, so no typed confirmation is needed.
func TestUserImpactEmptyAccount(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	u, err := s.CreateUser(ctx, "new", "password123", RoleRequester, false)
	if err != nil {
		t.Fatal(err)
	}
	imp, err := s.DeletionImpact(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if imp != (UserImpact{}) {
		t.Fatalf("impact = %+v, want all zero", imp)
	}
}
