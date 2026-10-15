package musicengine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The Acquire VPNCheck (KTD20). The torrent and Soulseek lanes may run only while their
// clients sit behind the tunnel. The attachment itself (network_mode container:gluetun, the
// firewall kill switch) is recorded at install (U16); this is the live half: gluetun's own
// control server saying the tunnel is up right now. It is the VPNCheck the lanes call before
// each search, so it answers from a short cache.

const (
	// DefaultGluetunControlURL is where the control server answers inside the compose network.
	DefaultGluetunControlURL = "http://gluetun:8000"
	// vpnProbeTimeout is the hard total deadline of one status request, dial through body.
	vpnProbeTimeout = 5 * time.Second
	// vpnCacheTTL is how long an answer is reused.
	vpnCacheTTL = 60 * time.Second
	// vpnMaxBody bounds what is read of the answer.
	vpnMaxBody = 64 << 10
)

// GluetunVPNConfig wires a GluetunVPN.
type GluetunVPNConfig struct {
	// URL returns the control server's base URL, read on each check so a changed setting
	// applies at once. Empty means DefaultGluetunControlURL.
	URL func() string
	// APIKey returns the optional X-API-Key value for a control server that requires one.
	APIKey func() string
	// HTTP is the client; nil means a default one. The deadline is the request context's,
	// not the client's.
	HTTP *http.Client
	Now  func() time.Time
	// Timeout overrides the 5 s probe deadline (tests).
	Timeout time.Duration
}

// GluetunVPN answers VPNCheck from gluetun's control server.
type GluetunVPN struct {
	cfg GluetunVPNConfig

	mu     sync.Mutex
	at     time.Time
	url    string
	key    string
	ok     bool
	reason string
}

// NewGluetunVPN builds the check.
func NewGluetunVPN(cfg GluetunVPNConfig) *GluetunVPN {
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = vpnProbeTimeout
	}
	return &GluetunVPN{cfg: cfg}
}

// Check is the VPNCheck. The VPN is OK only when gluetun answers 200 with status "running".
// A timeout, a refused connection, any other status code or any other status word is not OK,
// and the reason says which. Answers are cached for a minute; a check cut short by the
// caller's own context is not cached.
func (g *GluetunVPN) Check(ctx context.Context) (bool, string) {
	base := DefaultGluetunControlURL
	if g.cfg.URL != nil {
		if u := strings.TrimSpace(g.cfg.URL()); u != "" {
			base = u
		}
	}
	base = strings.TrimRight(base, "/")
	key := ""
	if g.cfg.APIKey != nil {
		key = strings.TrimSpace(g.cfg.APIKey())
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.cfg.Now()
	if !g.at.IsZero() && now.Sub(g.at) < vpnCacheTTL && g.url == base && g.key == key {
		return g.ok, g.reason
	}
	ok, reason := g.probe(ctx, base, key)
	if ctx.Err() != nil {
		return false, reason // the caller gave up; that is not news about the tunnel
	}
	g.at, g.url, g.key, g.ok, g.reason = now, base, key, ok, reason
	return ok, reason
}

func (g *GluetunVPN) probe(ctx context.Context, base, key string) (bool, string) {
	ctx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()
	endpoint := base + "/v1/vpn/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, fmt.Sprintf("Couldn't build the VPN status request to %s: %v", base, err)
	}
	req.Header.Set("Accept", "application/json")
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	resp, err := g.cfg.HTTP.Do(req)
	if err != nil {
		return false, fmt.Sprintf("Couldn't reach the VPN status page at %s: %v", endpoint, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, vpnMaxBody))
	if err != nil {
		return false, fmt.Sprintf("Couldn't read the VPN status from %s (HTTP %d): %v", endpoint, resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Sprintf("The VPN status page at %s answered HTTP %d: %s", endpoint, resp.StatusCode, clip(strings.TrimSpace(string(body)), 200))
	}
	var st struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		return false, fmt.Sprintf("The VPN status page at %s sent something unreadable: %v: %s", endpoint, err, clip(strings.TrimSpace(string(body)), 200))
	}
	if st.Status != "running" {
		return false, fmt.Sprintf("The VPN is %q, not running (%s)", st.Status, endpoint)
	}
	return true, ""
}

var _ VPNCheck = (*GluetunVPN)(nil).Check
