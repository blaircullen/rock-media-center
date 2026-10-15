package auth

import (
	"context"
	"database/sql"
	"errors"
)

// UserImpact is what deleting an account would take with it, as counts only. It is shown
// to an admin before the delete.
type UserImpact struct {
	Requests          int  `json:"requests"`           // their requests (kept after delete)
	Sessions          int  `json:"sessions"`           // browser sign-ins
	PushSubscriptions int  `json:"push_subscriptions"` // phones/browsers getting alerts
	PlexLinked        bool `json:"plex_linked"`        // can sign straight back in with Plex
	JellyfinLinked    bool `json:"jellyfin_linked"`    // can sign straight back in with Jellyfin
}

// DeletionImpact counts what deleting user id would remove. Every query is a plain count
// keyed by user_id.
func (s *Service) DeletionImpact(ctx context.Context, id int64) (UserImpact, error) {
	var imp UserImpact
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(plex_id, '') != '', COALESCE(jellyfin_id, '') != '' FROM users WHERE id = ?`, id).Scan(&imp.PlexLinked, &imp.JellyfinLinked)
	if errors.Is(err, sql.ErrNoRows) {
		return imp, ErrNotFound
	}
	if err != nil {
		return imp, err
	}
	counts := []struct {
		q   string
		dst any
	}{
		{`SELECT COUNT(*) FROM requests WHERE requested_by = ?`, &imp.Requests},
		{`SELECT COUNT(*) FROM sessions WHERE user_id = ?`, &imp.Sessions},
		{`SELECT COUNT(*) FROM push_subscriptions WHERE user_id = ?`, &imp.PushSubscriptions},
	}
	for _, c := range counts {
		if err := s.db.QueryRowContext(ctx, c.q, id).Scan(c.dst); err != nil {
			return imp, err
		}
	}
	return imp, nil
}
