package spotify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Spotify's Web API changed on 2026-02-11 (Development Mode apps; confirmed against
// developer.spotify.com/documentation/web-api/references/changes/february-2026 and the
// reference pages for Get Playlist and Get Playlist Items):
//   - GET /playlists/{id}/tracks is replaced by GET /playlists/{id}/items (page limit max 50).
//   - Get Playlist renames tracks -> items; each entry's `track` becomes `item`.
//   - Playlist contents are returned only for playlists the CURRENT USER owns or collaborates
//     on. An app token (client credentials) has no user, so a Development Mode app should
//     expect ErrContentsUnavailable. Extended Quota Mode apps keep the old behaviour.
//   - Track.linked_from was removed (again, Development Mode only); external_ids (ISRC) was
//     removed and then reverted in March 2026.
//
// The decoders below accept both the new and the old field names, so one client works for
// either quota mode; Config.LegacyTracks picks the old endpoint.

// ErrContentsUnavailable means Spotify answered but would not show the playlist's items to an
// app token (403 on the items endpoint, or no items object in the playlist). The read is
// failed, never treated as an empty playlist (R5).
var ErrContentsUnavailable = errors.New("spotify did not return the playlist's items to this app token " +
	"(Development Mode apps can only read items of playlists owned by the authorizing user; " +
	"needs Extended Quota Mode or a user-authorized token)")

// ErrIncomplete means a read finished paging but could not be proven complete: the snapshot id
// moved, the total moved, or the entry count differs from the reported total.
var ErrIncomplete = errors.New("spotify playlist read was not complete")

// PlaylistInfo is the playlist's own metadata.
type PlaylistInfo struct {
	ID         string
	Name       string
	OwnerID    string
	SnapshotID string
	Total      int // item count the playlist reports
}

// Entry is one playlist item. Fields are copied as Spotify gave them; deciding what they mean
// (R6) is the engine's job.
type Entry struct {
	Position     int // zero-based index in the playlist
	AddedAt      string
	Type         string // "track", "episode", or "" when the item had no object
	TrackID      string // empty for local files and unavailable items
	ISRC         string // empty when Spotify gave none (always for local files)
	Name         string
	Artists      []string
	Album        string
	DurationMs   int
	Explicit     bool
	IsLocal      bool
	LinkedFromID string // set when the track was relinked: the id originally in the playlist
	// Unavailable is true when the item object is missing (removed from Spotify) or Spotify
	// marks it not playable in the market. Such entries still count toward the total.
	Unavailable bool
}

// Playlist is a complete, verified read.
type Playlist struct {
	PlaylistInfo
	Entries []Entry
}

// ParsePlaylistID accepts a bare id, a spotify:playlist: URI or an open.spotify.com URL and
// returns the id. It rejects anything that is not a plain alphanumeric id.
func ParsePlaylistID(s string) (string, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "spotify:playlist:") {
		s = strings.TrimPrefix(s, "spotify:playlist:")
	} else if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return "", fmt.Errorf("spotify playlist url %q: %w", s, err)
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) < 2 || parts[len(parts)-2] != "playlist" {
			return "", fmt.Errorf("spotify playlist url %q has no /playlist/<id> path", s)
		}
		s = parts[len(parts)-1]
	}
	if s == "" {
		return "", errors.New("spotify playlist id is empty")
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return "", fmt.Errorf("spotify playlist id %q has invalid characters", s)
		}
	}
	return s, nil
}

type rawTotal struct {
	Total *int `json:"total"`
}

type rawPlaylist struct {
	Name       string `json:"name"`
	SnapshotID string `json:"snapshot_id"`
	Owner      struct {
		ID string `json:"id"`
	} `json:"owner"`
	Items  *rawTotal `json:"items"`  // current name
	Tracks *rawTotal `json:"tracks"` // pre-2026 name
}

type rawTrack struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Name       string `json:"name"`
	DurationMs int    `json:"duration_ms"`
	Explicit   bool   `json:"explicit"`
	IsLocal    bool   `json:"is_local"`
	IsPlayable *bool  `json:"is_playable"`
	Artists    []struct {
		Name string `json:"name"`
	} `json:"artists"`
	Album struct {
		Name string `json:"name"`
	} `json:"album"`
	ExternalIDs struct {
		ISRC string `json:"isrc"`
	} `json:"external_ids"`
	LinkedFrom *struct {
		ID string `json:"id"`
	} `json:"linked_from"`
}

type rawItem struct {
	AddedAt string    `json:"added_at"`
	IsLocal bool      `json:"is_local"`
	Item    *rawTrack `json:"item"`  // current name
	Track   *rawTrack `json:"track"` // pre-2026 name
}

type rawPage struct {
	Offset int       `json:"offset"`
	Total  *int      `json:"total"`
	Next   *string   `json:"next"`
	Items  []rawItem `json:"items"`
}

func (it rawItem) toEntry(pos int) Entry {
	e := Entry{Position: pos, AddedAt: it.AddedAt, IsLocal: it.IsLocal}
	t := it.Item
	if t == nil {
		t = it.Track
	}
	if t == nil {
		e.Unavailable = true
		return e
	}
	e.Type = t.Type
	e.TrackID = t.ID
	e.ISRC = strings.ToUpper(strings.TrimSpace(t.ExternalIDs.ISRC))
	e.Name = t.Name
	e.Album = t.Album.Name
	e.DurationMs = t.DurationMs
	e.Explicit = t.Explicit
	e.IsLocal = e.IsLocal || t.IsLocal
	for _, a := range t.Artists {
		e.Artists = append(e.Artists, a.Name)
	}
	if t.LinkedFrom != nil {
		e.LinkedFromID = t.LinkedFrom.ID
	}
	if t.IsPlayable != nil && !*t.IsPlayable {
		e.Unavailable = true
	}
	return e
}

// playlistInfo reads the playlist's metadata (snapshot id, name, owner, item count).
func (c *Client) playlistInfo(ctx context.Context, id string) (PlaylistInfo, error) {
	q := url.Values{}
	q.Set("fields", "snapshot_id,name,owner(id),tracks(total),items(total)")
	q.Set("market", c.market)
	op := "GET /v1/playlists/" + id
	var raw rawPlaylist
	if err := c.getJSON(ctx, op, c.apiBase+"/v1/playlists/"+url.PathEscape(id)+"?"+q.Encode(), &raw); err != nil {
		return PlaylistInfo{}, err
	}
	if raw.SnapshotID == "" {
		return PlaylistInfo{}, fmt.Errorf("spotify %s: response had no snapshot_id", op)
	}
	tot := raw.Items
	if tot == nil || tot.Total == nil {
		tot = raw.Tracks
	}
	if tot == nil || tot.Total == nil {
		// Development Mode answers metadata only for playlists the app token cannot open.
		return PlaylistInfo{}, fmt.Errorf("%w: GET %s returned no items total (snapshot %s)", ErrContentsUnavailable, op, raw.SnapshotID)
	}
	return PlaylistInfo{ID: id, Name: raw.Name, OwnerID: raw.Owner.ID, SnapshotID: raw.SnapshotID, Total: *tot.Total}, nil
}

// ReadPlaylist reads the whole playlist or returns an error and no entries. The read is
// complete only when (1) the playlist's snapshot id read before paging equals the one read
// after, (2) the reported total is the same in both reads and on every page, (3) page offsets
// are contiguous, and (4) the number of entries equals the total. Anything else is an error
// (wrapping ErrIncomplete where the read finished but cannot be trusted) so a partial or
// shifting playlist is never mistaken for an empty or smaller one (R5).
func (c *Client) ReadPlaylist(ctx context.Context, playlistID string) (*Playlist, error) {
	id, err := ParsePlaylistID(playlistID)
	if err != nil {
		return nil, err
	}
	before, err := c.playlistInfo(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("reading playlist %s (before paging): %w", id, err)
	}

	suffix := "items"
	if c.legacyTracks {
		suffix = "tracks"
	}
	op := "GET /v1/playlists/" + id + "/" + suffix
	q := url.Values{}
	q.Set("limit", fmt.Sprint(c.pageSize))
	q.Set("offset", "0")
	q.Set("market", c.market)
	next := c.apiBase + "/v1/playlists/" + url.PathEscape(id) + "/" + suffix + "?" + q.Encode()

	entries := make([]Entry, 0, before.Total)
	seen := map[string]bool{}
	for next != "" {
		if seen[next] {
			return nil, fmt.Errorf("%w: %s: page link loop at %s", ErrIncomplete, op, next)
		}
		seen[next] = true
		if len(seen) > before.Total/c.pageSize+10 {
			return nil, fmt.Errorf("%w: %s: more pages than %d items can fill", ErrIncomplete, op, before.Total)
		}
		var page rawPage
		if err := c.getJSON(ctx, op, next, &page); err != nil {
			var ae *APIError
			if errors.As(err, &ae) && ae.Status == http.StatusForbidden {
				return nil, fmt.Errorf("reading playlist %s: %w: %v", id, ErrContentsUnavailable, err)
			}
			return nil, fmt.Errorf("reading playlist %s (page at offset %d): %w", id, len(entries), err)
		}
		if page.Offset != len(entries) {
			return nil, fmt.Errorf("%w: %s: page offset %d, expected %d", ErrIncomplete, op, page.Offset, len(entries))
		}
		if page.Total == nil || *page.Total != before.Total {
			return nil, fmt.Errorf("%w: %s: page total %s differs from playlist total %d", ErrIncomplete, op, fmtTotal(page.Total), before.Total)
		}
		for _, it := range page.Items {
			entries = append(entries, it.toEntry(len(entries)))
		}
		next = ""
		if page.Next != nil && *page.Next != "" {
			if len(page.Items) == 0 {
				return nil, fmt.Errorf("%w: %s: empty page at offset %d still links to a next page", ErrIncomplete, op, page.Offset)
			}
			next, err = c.sameHostURL(*page.Next)
			if err != nil {
				return nil, fmt.Errorf("reading playlist %s: %w", id, err)
			}
		}
	}

	after, err := c.playlistInfo(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("reading playlist %s (after paging): %w", id, err)
	}
	if after.SnapshotID != before.SnapshotID {
		return nil, fmt.Errorf("%w: playlist %s changed while paging (snapshot %s -> %s)", ErrIncomplete, id, before.SnapshotID, after.SnapshotID)
	}
	if after.Total != before.Total {
		return nil, fmt.Errorf("%w: playlist %s total changed while paging (%d -> %d)", ErrIncomplete, id, before.Total, after.Total)
	}
	if len(entries) != before.Total {
		return nil, fmt.Errorf("%w: playlist %s returned %d entries, Spotify reports %d", ErrIncomplete, id, len(entries), before.Total)
	}
	return &Playlist{PlaylistInfo: before, Entries: entries}, nil
}

// sameHostURL resolves a `next` link and refuses one that points at another host, so the
// bearer token is never sent anywhere but the configured API.
func (c *Client) sameHostURL(next string) (string, error) {
	base, err := url.Parse(c.apiBase)
	if err != nil {
		return "", err
	}
	u, err := base.Parse(next)
	if err != nil {
		return "", fmt.Errorf("bad next link %q: %w", next, err)
	}
	if u.Host != c.apiHost {
		return "", fmt.Errorf("next link %q points at %s, not %s", next, u.Host, c.apiHost)
	}
	return u.String(), nil
}

func fmtTotal(t *int) string {
	if t == nil {
		return "(missing)"
	}
	return fmt.Sprint(*t)
}
