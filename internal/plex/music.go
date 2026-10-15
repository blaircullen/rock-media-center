package plex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Music section item type, as /library/sections/{key}/all?type= takes it.
const TypeTrack = 10

const musicPageSize = 500

// Track is one music track in a Plex music section.
type Track struct {
	RatingKey string
	Title     string
	Artist    string // grandparentTitle
	Album     string // parentTitle
	// UserRating is Plex's 0-10 scale (10 = five stars); 0 means unrated.
	UserRating float64
	ViewCount  int
	// Files are the media part file paths as the Plex server sees them.
	Files []string
}

// flexFloat tolerates a numeric field arriving as a JSON number or a quoted string.
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	*f = flexFloat(v)
	return nil
}

type rawTrack struct {
	RatingKey        string    `json:"ratingKey"`
	Title            string    `json:"title"`
	GrandparentTitle string    `json:"grandparentTitle"`
	ParentTitle      string    `json:"parentTitle"`
	UserRating       flexFloat `json:"userRating"`
	ViewCount        flexInt   `json:"viewCount"`
	Media            []struct {
		Part []struct {
			File string `json:"file"`
		} `json:"Part"`
	} `json:"Media"`
}

func (m rawTrack) track() Track {
	t := Track{
		RatingKey: m.RatingKey, Title: m.Title, Artist: m.GrandparentTitle, Album: m.ParentTitle,
		UserRating: float64(m.UserRating), ViewCount: int(m.ViewCount),
	}
	for _, media := range m.Media {
		for _, p := range media.Part {
			if p.File != "" {
				t.Files = append(t.Files, p.File)
			}
		}
	}
	return t
}

type trackPage struct {
	MediaContainer struct {
		Size      flexInt    `json:"size"`
		TotalSize flexInt    `json:"totalSize"`
		Metadata  []rawTrack `json:"Metadata"`
	} `json:"MediaContainer"`
}

// Tracks lists every track in a music section, 500 per request.
func (c *Client) Tracks(ctx context.Context, sectionKey string) ([]Track, error) {
	if sectionKey == "" {
		return nil, errors.New("no library section")
	}
	var out []Track
	// A runaway totalSize can't loop forever: 400 pages is 200,000 tracks.
	for start, page := 0, 0; page < 400; page++ {
		var r trackPage
		path := fmt.Sprintf("/library/sections/%s/all?type=%d&X-Plex-Container-Start=%d&X-Plex-Container-Size=%d",
			url.PathEscape(sectionKey), TypeTrack, start, musicPageSize)
		if err := c.get(ctx, path, &r); err != nil {
			return nil, fmt.Errorf("plex list tracks (section %s, start %d): %w", sectionKey, start, err)
		}
		for _, m := range r.MediaContainer.Metadata {
			out = append(out, m.track())
		}
		n := len(r.MediaContainer.Metadata)
		start += n
		if total := int(r.MediaContainer.TotalSize); n == 0 || (total > 0 && start >= total) || (total == 0 && n < musicPageSize) {
			break
		}
	}
	return out, nil
}

// SearchTracks finds tracks whose title matches title (Plex's own matching) and, when
// artist is non-empty, whose artist equals it case-insensitively.
func (c *Client) SearchTracks(ctx context.Context, sectionKey, artist, title string) ([]Track, error) {
	if sectionKey == "" {
		return nil, errors.New("no library section")
	}
	var r trackPage
	path := fmt.Sprintf("/library/sections/%s/search?type=%d&title=%s", url.PathEscape(sectionKey), TypeTrack, queryEscape(title))
	if err := c.get(ctx, path, &r); err != nil {
		return nil, fmt.Errorf("plex search tracks (section %s, title %q): %w", sectionKey, title, err)
	}
	var out []Track
	for _, m := range r.MediaContainer.Metadata {
		t := m.track()
		if artist != "" && !strings.EqualFold(t.Artist, artist) {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// TrackIndexed reports whether Plex has a track under the given artist and album whose
// file is exactly path (the path as the Plex server sees it). It searches by title, so
// the caller passes the title it expects; false with a nil error means "not indexed yet".
func (c *Client) TrackIndexed(ctx context.Context, sectionKey, artist, album, title, path string) (bool, error) {
	tracks, err := c.SearchTracks(ctx, sectionKey, artist, title)
	if err != nil {
		return false, err
	}
	for _, t := range tracks {
		if !strings.EqualFold(t.Album, album) {
			continue
		}
		for _, f := range t.Files {
			if f == path {
				return true, nil
			}
		}
	}
	return false, nil
}

// SetRating sets a track's rating on Plex's 0-10 scale (10 = five stars).
// NOTE: write endpoint shapes (PUT /:/rate here, the playlist writes in playlists.go) are
// from Plex's public API conventions and are to be confirmed against Beast's Plex at install.
func (c *Client) SetRating(ctx context.Context, ratingKey string, rating float64) error {
	if ratingKey == "" {
		return errors.New("no rating key")
	}
	if rating < 0 || rating > 10 {
		return fmt.Errorf("rating %v out of range 0-10", rating)
	}
	path := "/:/rate?key=" + url.QueryEscape(ratingKey) + "&identifier=com.plexapp.plugins.library&rating=" +
		strconv.FormatFloat(rating, 'f', -1, 64)
	if err := c.send(ctx, http.MethodPut, path, nil); err != nil {
		return fmt.Errorf("plex set rating (key %s): %w", ratingKey, err)
	}
	return nil
}

// send performs a write (PUT/POST/DELETE) and, when out is non-nil, decodes the JSON
// answer into it. Unlike do, a failure keeps the status and a slice of the response body,
// so a wrong endpoint shape shows up in the error instead of a bare "HTTP 400".
func (c *Client) send(ctx context.Context, method, path string, out any) error {
	if c.base == "" || c.token == "" {
		return ErrNotConfigured
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Token", c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return ErrUnauthorized
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("plex %s %s: reading response (HTTP %d): %w", method, strings.SplitN(path, "?", 2)[0], resp.StatusCode, err)
	}
	if resp.StatusCode >= 300 {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return fmt.Errorf("plex %s %s returned HTTP %d: %s", method, strings.SplitN(path, "?", 2)[0], resp.StatusCode, snippet)
	}
	if out == nil || len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("plex %s %s: decoding response: %w", method, strings.SplitN(path, "?", 2)[0], err)
	}
	return nil
}
