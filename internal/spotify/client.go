// Package spotify is a thin, read-only client for the Spotify Web API, used by the music
// engine to read the one playlist Blair watches. It authenticates with the client-credentials
// flow (an app token, no user), so it can only see what an app token can see; see ReadPlaylist
// for what that means after Spotify's February 2026 Development Mode changes.
//
// Credentials and base URLs are plain constructor values. The wiring unit reads the
// credentials from the API-key store; nothing here reads settings or the environment.
package spotify

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
	"sync"
	"time"
)

const (
	// DefaultAPIBase and DefaultAccountsBase are the production Spotify hosts.
	DefaultAPIBase      = "https://api.spotify.com"
	DefaultAccountsBase = "https://accounts.spotify.com"

	// DefaultMarket is sent on playlist reads so Spotify applies track relinking (the only way
	// linked_from can appear). See Config.Market.
	DefaultMarket = "US"

	// tokenSkew is how long before the stated expiry a cached token is treated as expired, so a
	// token never dies mid-request.
	tokenSkew = 30 * time.Second

	maxErrBody  = 2048
	maxJSONBody = 8 << 20
)

// ErrNotConfigured means the client id or secret is empty.
var ErrNotConfigured = errors.New("spotify is not configured (client id and secret are required)")

// APIError is a non-2xx answer from Spotify (or its token endpoint). It keeps the status and
// the start of the body so a failed read can be diagnosed later without guessing.
type APIError struct {
	Op         string // what the client was doing, e.g. "token" or "GET /v1/playlists/x/items"
	Status     int
	Body       string // first maxErrBody bytes of the response body
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("spotify %s: HTTP %d", e.Op, e.Status)
	if e.RetryAfter > 0 {
		msg += fmt.Sprintf(" (retry after %s)", e.RetryAfter)
	}
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// Config holds everything New needs. Only ClientID and ClientSecret are required.
type Config struct {
	ClientID     string
	ClientSecret string
	// APIBase and AccountsBase default to the production hosts; tests point them at httptest
	// servers. A trailing slash is trimmed.
	APIBase      string
	AccountsBase string
	// Market is the ISO 3166-1 alpha-2 country sent as `market` on playlist reads. Default "US".
	// Decision: `market` is always sent. Spotify only returns linked_from (the original id of a
	// relinked track) when a market is given, and says content is "unavailable for the client" when
	// an app token supplies no market at all. A relinked entry is reported with LinkedFromID set so
	// the engine can send it to Blair (R6) rather than trust the replacement id.
	Market string
	// PageSize is the items page size, 1..50. Default 50, the maximum Spotify's current
	// /playlists/{id}/items allows (the plan's 100 was the old /tracks maximum).
	PageSize int
	// LegacyTracks reads GET /playlists/{id}/tracks instead of /items. Only apps in Spotify's
	// Extended Quota Mode still have it; Development Mode apps get 404 there.
	LegacyTracks bool
	// HTTPClient is optional; the default has a 20 s timeout.
	HTTPClient *http.Client
	// Now is optional (tests inject a fake clock for token expiry).
	Now func() time.Time
}

// Client talks to the Spotify Web API with an app token.
type Client struct {
	clientID, clientSecret string
	apiBase, accountsBase  string
	apiHost                string
	market                 string
	pageSize               int
	legacyTracks           bool
	http                   *http.Client
	now                    func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

// New builds a client. It returns ErrNotConfigured when credentials are missing.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.ClientID) == "" || strings.TrimSpace(cfg.ClientSecret) == "" {
		return nil, ErrNotConfigured
	}
	c := &Client{
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		apiBase:      strings.TrimRight(orDefault(cfg.APIBase, DefaultAPIBase), "/"),
		accountsBase: strings.TrimRight(orDefault(cfg.AccountsBase, DefaultAccountsBase), "/"),
		market:       orDefault(cfg.Market, DefaultMarket),
		pageSize:     cfg.PageSize,
		legacyTracks: cfg.LegacyTracks,
		http:         cfg.HTTPClient,
		now:          cfg.Now,
	}
	if c.pageSize < 1 || c.pageSize > 50 {
		c.pageSize = 50
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 20 * time.Second}
	}
	if c.now == nil {
		c.now = time.Now
	}
	u, err := url.Parse(c.apiBase)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("spotify: bad API base URL %q", cfg.APIBase)
	}
	c.apiHost = u.Host
	return c, nil
}

func orDefault(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return v
}

// accessToken returns the cached app token, fetching a new one when none is cached or it is
// within tokenSkew of expiry.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Before(c.expires.Add(-tokenSkew)) {
		return c.token, nil
	}
	return c.fetchTokenLocked(ctx)
}

// invalidate drops the cached token if it is still the one that just got a 401, so a
// concurrent caller's fresher token is not thrown away.
func (c *Client) invalidate(stale string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == stale {
		c.token = ""
	}
}

func (c *Client) fetchTokenLocked(ctx context.Context) (string, error) {
	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.accountsBase+"/api/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.clientID, c.clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("spotify token request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", newAPIError("token", resp)
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBody)).Decode(&tr); err != nil {
		return "", fmt.Errorf("spotify token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", errors.New("spotify token response had no access_token")
	}
	c.token = tr.AccessToken
	c.expires = c.now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return c.token, nil
}

func newAPIError(op string, resp *http.Response) *APIError {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
	e := &APIError{Op: op, Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	if s := resp.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			e.RetryAfter = time.Duration(n) * time.Second
		}
	}
	return e
}

// getJSON GETs fullURL with the app token and decodes the JSON body into out. A 401 drops the
// cached token, fetches a fresh one and retries exactly once; a second 401 is returned as an
// error. op labels errors (the path, never the token).
func (c *Client) getJSON(ctx context.Context, op, fullURL string, out any) error {
	for attempt := 0; ; attempt++ {
		tok, err := c.accessToken(ctx)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("spotify %s: %w", op, err)
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrBody))
			resp.Body.Close()
			c.invalidate(tok)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			e := newAPIError(op, resp)
			resp.Body.Close()
			return e
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, maxJSONBody)).Decode(out)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("spotify %s: decode: %w", op, err)
		}
		return nil
	}
}
