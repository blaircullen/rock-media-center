package httpapi

import (
	"net/http"

	"github.com/tristenlammi/arrmada/internal/auth"
)

// personRow is one person on Settings → People & access: their account, how they get in,
// and when they were last around.
type personRow struct {
	ID                int64          `json:"id"`
	Username          string         `json:"username"`
	Role              auth.Role      `json:"role"`
	Disabled          bool           `json:"disabled"`
	CreatedAt         string         `json:"created_at,omitempty"`
	AutoApproveMovie  bool           `json:"auto_approve_movie"`
	AutoApproveSeries bool           `json:"auto_approve_series"`
	PlexLinked        bool           `json:"plex_linked"`
	PlexUsername      string         `json:"plex_username,omitempty"`
	PlexOnly          bool           `json:"plex_only,omitempty"`
	PlexBlocked       bool           `json:"plex_blocked,omitempty"`
	JellyfinLinked    bool           `json:"jellyfin_linked"`
	JellyfinUsername  string         `json:"jellyfin_username,omitempty"`
	JellyfinOnly      bool           `json:"jellyfin_only,omitempty"`
	JellyfinBlocked   bool           `json:"jellyfin_blocked,omitempty"`
	Quota             auth.UserQuota `json:"quota"`
	// Sessions is how many browsers they're signed in on (only they see which).
	Sessions int `json:"sessions"`
	// Unix ms, 0 = never: the last use of the web app.
	WebLast int64 `json:"web_last"`
	// LastSeen is when they were last around (today the same as WebLast).
	LastSeen int64 `json:"last_seen"`
}

// handlePeople — GET /api/v1/people (admin): everyone once, with role, auto-approve, Plex
// link, last seen and sessions, so one list replaces Users and the Plex blocks.
func (a *api) handlePeople(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	users, err := a.deps.Auth.ListUsers(ctx)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "could not list people")
		return
	}
	quotas, err := a.deps.Auth.Quotas(ctx)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "could not list people")
		return
	}
	activity, err := a.deps.Auth.SessionActivityByUser(ctx)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "could not list people")
		return
	}
	blocked := map[string]bool{}
	for _, b := range a.plexBlocks(ctx) {
		blocked[b.PlexID] = true
	}
	jfBlocked := map[string]bool{}
	for _, b := range a.jellyfinBlocks(ctx) {
		jfBlocked[b.JellyfinID] = true
	}

	out := make([]personRow, 0, len(users))
	for _, u := range users {
		p := personRow{
			ID: u.ID, Username: u.Username, Role: u.Role, Disabled: u.Disabled, CreatedAt: u.CreatedAt,
			AutoApproveMovie: u.AutoApproveMovie, AutoApproveSeries: u.AutoApproveSeries,
			PlexLinked: u.PlexLinked, PlexUsername: u.PlexUsername, PlexOnly: u.PlexOnly,
			JellyfinLinked: u.JellyfinLinked, JellyfinUsername: u.JellyfinUsername, JellyfinOnly: u.JellyfinOnly,
			Quota: auth.DefaultQuota,
		}
		if q, ok := quotas[u.ID]; ok {
			p.Quota = q
		}
		if u.PlexLinked && len(blocked) > 0 {
			p.PlexBlocked = blocked[a.deps.Auth.PlexIDForUser(ctx, u.ID)]
		}
		if u.JellyfinLinked && len(jfBlocked) > 0 {
			p.JellyfinBlocked = jfBlocked[a.deps.Auth.JellyfinIDForUser(ctx, u.ID)]
		}
		if s, ok := activity[u.ID]; ok {
			p.Sessions = s.Sessions
			if !s.LastSeen.IsZero() {
				p.WebLast = s.LastSeen.UnixMilli()
			}
		}
		p.LastSeen = p.WebLast
		out = append(out, p)
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"people": out})
}
