package plex

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Playlist is one audio playlist.
type Playlist struct {
	RatingKey string
	Title     string
	Smart     bool
}

// PlaylistItem is one entry of a playlist. PlaylistItemID identifies the entry itself
// (what removal needs); RatingKey identifies the track.
type PlaylistItem struct {
	PlaylistItemID string
	RatingKey      string
	Title          string
	Artist         string
	Album          string
}

type rawPlaylist struct {
	RatingKey string  `json:"ratingKey"`
	Title     string  `json:"title"`
	Smart     flexStr `json:"smart"` // 1/true/"1" depending on the server
}

// Playlists lists audio playlists, leaving out smart playlists (such as "All Music").
func (c *Client) Playlists(ctx context.Context) ([]Playlist, error) {
	var r struct {
		MediaContainer struct {
			Metadata []rawPlaylist `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := c.get(ctx, "/playlists?playlistType=audio", &r); err != nil {
		return nil, fmt.Errorf("plex list playlists: %w", err)
	}
	var out []Playlist
	for _, p := range r.MediaContainer.Metadata {
		if p.Smart.on() {
			continue
		}
		out = append(out, Playlist{RatingKey: p.RatingKey, Title: p.Title})
	}
	return out, nil
}

// PlaylistItems reads a playlist's entries in order.
func (c *Client) PlaylistItems(ctx context.Context, playlistKey string) ([]PlaylistItem, error) {
	if playlistKey == "" {
		return nil, errors.New("no playlist key")
	}
	var r struct {
		MediaContainer struct {
			Metadata []struct {
				RatingKey        string  `json:"ratingKey"`
				PlaylistItemID   flexStr `json:"playlistItemID"`
				Title            string  `json:"title"`
				GrandparentTitle string  `json:"grandparentTitle"`
				ParentTitle      string  `json:"parentTitle"`
			} `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := c.get(ctx, "/playlists/"+url.PathEscape(playlistKey)+"/items", &r); err != nil {
		return nil, fmt.Errorf("plex read playlist %s: %w", playlistKey, err)
	}
	out := make([]PlaylistItem, 0, len(r.MediaContainer.Metadata))
	for _, m := range r.MediaContainer.Metadata {
		out = append(out, PlaylistItem{
			PlaylistItemID: string(m.PlaylistItemID), RatingKey: m.RatingKey,
			Title: m.Title, Artist: m.GrandparentTitle, Album: m.ParentTitle,
		})
	}
	return out, nil
}

// itemsURI is the server://{machineId}/... uri Plex takes to name tracks by rating key.
func (c *Client) itemsURI(ctx context.Context, ratingKeys []string) (string, error) {
	if len(ratingKeys) == 0 {
		return "", errors.New("no tracks given")
	}
	id, err := c.Identity(ctx)
	if err != nil {
		return "", fmt.Errorf("plex identity: %w", err)
	}
	if id.MachineIdentifier == "" {
		return "", errors.New("plex identity has no machineIdentifier")
	}
	return "server://" + id.MachineIdentifier + "/com.plexapp.plugins.library/library/metadata/" + strings.Join(ratingKeys, ","), nil
}

// CreatePlaylist creates a non-smart audio playlist holding the given tracks (Plex needs
// at least one to create) and returns its rating key.
// NOTE: write endpoint shapes are to be confirmed against Beast's Plex at install.
func (c *Client) CreatePlaylist(ctx context.Context, title string, ratingKeys []string) (string, error) {
	if title == "" {
		return "", errors.New("no playlist title")
	}
	uri, err := c.itemsURI(ctx, ratingKeys)
	if err != nil {
		return "", err
	}
	var r struct {
		MediaContainer struct {
			Metadata []rawPlaylist `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	path := "/playlists?type=audio&smart=0&title=" + queryEscape(title) + "&uri=" + queryEscape(uri)
	if err := c.send(ctx, http.MethodPost, path, &r); err != nil {
		return "", fmt.Errorf("plex create playlist %q: %w", title, err)
	}
	if len(r.MediaContainer.Metadata) == 0 || r.MediaContainer.Metadata[0].RatingKey == "" {
		return "", fmt.Errorf("plex create playlist %q: response named no playlist", title)
	}
	return r.MediaContainer.Metadata[0].RatingKey, nil
}

// AddToPlaylist appends tracks to a playlist.
func (c *Client) AddToPlaylist(ctx context.Context, playlistKey string, ratingKeys []string) error {
	if playlistKey == "" {
		return errors.New("no playlist key")
	}
	uri, err := c.itemsURI(ctx, ratingKeys)
	if err != nil {
		return err
	}
	path := "/playlists/" + url.PathEscape(playlistKey) + "/items?uri=" + queryEscape(uri)
	if err := c.send(ctx, http.MethodPut, path, nil); err != nil {
		return fmt.Errorf("plex add to playlist %s: %w", playlistKey, err)
	}
	return nil
}

// RemoveFromPlaylist removes one entry, named by its PlaylistItemID (not the track's key).
func (c *Client) RemoveFromPlaylist(ctx context.Context, playlistKey, playlistItemID string) error {
	if playlistKey == "" || playlistItemID == "" {
		return errors.New("playlist key and item id are required")
	}
	path := "/playlists/" + url.PathEscape(playlistKey) + "/items/" + url.PathEscape(playlistItemID)
	if err := c.send(ctx, http.MethodDelete, path, nil); err != nil {
		return fmt.Errorf("plex remove from playlist %s: %w", playlistKey, err)
	}
	return nil
}
