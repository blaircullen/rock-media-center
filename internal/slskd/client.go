// Package slskd is a client for the slskd (Soulseek) HTTP API: start a search, poll it, read the
// flattened file responses, queue a download from one user and poll that transfer.
//
// Errors are split so the Soulseek lane can tell "unavailable" from "searched, nothing":
//   - ErrUnavailable: slskd could not be reached (connection refused, timeout, 502/503/504 from a proxy).
//   - ErrAuth: slskd answered 401/403, so the API key is wrong or missing.
//   - ErrNotFound: slskd answered 404 for a search or transfer that was asked for by id.
//
// A search that completes with no responses is NOT an error: Search returns an empty slice and nil.
// Routes follow slskd's /api/v0 surface (the same ones the getmusic scripts use).
package slskd

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	ErrUnavailable = errors.New("slskd: unavailable")
	ErrAuth        = errors.New("slskd: authentication failed")
	ErrNotFound    = errors.New("slskd: not found")
)

// APIError is any other non-2xx answer; it keeps the status and a body excerpt so a failure is diagnosable.
type APIError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("slskd: %s %s returned %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// File is one file offered in a search response, flattened with its owner's details.
type File struct {
	Username    string
	Filename    string // full remote path as slskd reports it; pass back unchanged to Enqueue
	Size        int64
	Extension   string
	BitRate     int
	BitDepth    int
	SampleRate  int
	LengthSec   int
	UploadSpeed int64
	QueueLength int
	FreeSlot    bool
}

// SearchOptions tunes one search. Zero values take the defaults noted.
type SearchOptions struct {
	Timeout       time.Duration // slskd-side search window; default 20s
	ResponseLimit int           // default 100
	FileLimit     int           // default 5000
}

// Phase is the coarse state of a transfer.
type Phase string

const (
	PhasePending   Phase = "pending"   // queued, requested or in progress
	PhaseSucceeded Phase = "succeeded" // completed successfully
	PhaseFailed    Phase = "failed"    // completed unsuccessfully (errored, rejected, timed out, cancelled)
)

// Transfer is one download as slskd reports it.
type Transfer struct {
	ID               string
	Username         string
	Filename         string
	State            string // raw slskd state, e.g. "Completed, Succeeded"
	Phase            Phase
	Size             int64
	BytesTransferred int64
	Exception        string
}

// Client talks to one slskd instance.
type Client struct {
	base string
	key  string
	http *http.Client
	// PollInterval is the delay between polls in Search and WaitDownload; default 2s.
	PollInterval time.Duration
}

// New builds a client from a base URL (scheme://host:port, with or without /api/v0) and an API key.
func New(baseURL, apiKey string) *Client {
	b := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	b = strings.TrimSuffix(b, "/api/v0")
	return &Client{
		base:         b,
		key:          strings.TrimSpace(apiKey),
		http:         &http.Client{Timeout: 30 * time.Second},
		PollInterval: 2 * time.Second,
	}
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("slskd: encode %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/api/v0"+path, body)
	if err != nil {
		return fmt.Errorf("slskd: build %s %s: %w", method, path, err)
	}
	req.Header.Set("X-API-Key", c.key)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %s %s: %v", ErrUnavailable, method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: read %s %s: %v", ErrUnavailable, method, path, err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %s %s returned %d: %s", ErrAuth, method, path, resp.StatusCode, excerpt(raw))
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %s %s: %s", ErrNotFound, method, path, excerpt(raw))
	case resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout:
		return fmt.Errorf("%w: %s %s returned %d: %s", ErrUnavailable, method, path, resp.StatusCode, excerpt(raw))
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return &APIError{Method: method, Path: path, Status: resp.StatusCode, Body: excerpt(raw)}
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("slskd: decode %s %s: %w (body: %s)", method, path, err, excerpt(raw))
	}
	return nil
}

func excerpt(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

func (c *Client) sleep(ctx context.Context) error {
	d := c.PollInterval
	if d <= 0 {
		d = 2 * time.Second
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("slskd: generate search id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

type searchFile struct {
	Filename   string `json:"filename"`
	Size       int64  `json:"size"`
	Extension  string `json:"extension"`
	BitRate    int    `json:"bitRate"`
	BitDepth   int    `json:"bitDepth"`
	SampleRate int    `json:"sampleRate"`
	Length     int    `json:"length"`
	IsLocked   bool   `json:"isLocked"`
}

type searchResponse struct {
	Username          string       `json:"username"`
	UploadSpeed       int64        `json:"uploadSpeed"`
	QueueLength       int          `json:"queueLength"`
	HasFreeUploadSlot bool         `json:"hasFreeUploadSlot"`
	Files             []searchFile `json:"files"`
}

type searchState struct {
	ID        string           `json:"id"`
	State     string           `json:"state"`
	Responses []searchResponse `json:"responses"`
}

// StartSearch begins a search and returns its id.
func (c *Client) StartSearch(ctx context.Context, query string, opt SearchOptions) (string, error) {
	if strings.TrimSpace(query) == "" {
		return "", errors.New("slskd: empty search query")
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 20 * time.Second
	}
	if opt.ResponseLimit <= 0 {
		opt.ResponseLimit = 100
	}
	if opt.FileLimit <= 0 {
		opt.FileLimit = 5000
	}
	id, err := newID()
	if err != nil {
		return "", err
	}
	payload := map[string]any{
		"id":              id,
		"searchText":      query,
		"searchTimeout":   opt.Timeout.Milliseconds(),
		"responseLimit":   opt.ResponseLimit,
		"fileLimit":       opt.FileLimit,
		"filterResponses": true,
	}
	if err := c.do(ctx, http.MethodPost, "/searches", payload, nil); err != nil {
		return "", err
	}
	return id, nil
}

// Search runs a search to completion and returns the flattened, unlocked files. Completing with
// no responses returns an empty slice and nil. If the window passes without slskd marking the
// search complete, the responses collected so far are returned.
func (c *Client) Search(ctx context.Context, query string, opt SearchOptions) ([]File, error) {
	id, err := c.StartSearch(ctx, query, opt)
	if err != nil {
		return nil, err
	}
	window := opt.Timeout
	if window <= 0 {
		window = 20 * time.Second
	}
	deadline := time.Now().Add(window + 15*time.Second)
	for {
		var st searchState
		if err := c.do(ctx, http.MethodGet, "/searches/"+url.PathEscape(id)+"?includeResponses=true", nil, &st); err != nil {
			return nil, err
		}
		if strings.Contains(st.State, "Completed") || !time.Now().Before(deadline) {
			return flatten(st.Responses), nil
		}
		if err := c.sleep(ctx); err != nil {
			return nil, err
		}
	}
}

func flatten(rs []searchResponse) []File {
	out := []File{}
	for _, r := range rs {
		for _, f := range r.Files {
			if f.IsLocked {
				continue
			}
			out = append(out, File{
				Username: r.Username, Filename: f.Filename, Size: f.Size, Extension: f.Extension,
				BitRate: f.BitRate, BitDepth: f.BitDepth, SampleRate: f.SampleRate, LengthSec: f.Length,
				UploadSpeed: r.UploadSpeed, QueueLength: r.QueueLength, FreeSlot: r.HasFreeUploadSlot,
			})
		}
	}
	return out
}

// Enqueue queues one file for download from a user. A "failed" entry in the answer is an error.
func (c *Client) Enqueue(ctx context.Context, username, filename string, size int64) error {
	if username == "" || filename == "" {
		return errors.New("slskd: enqueue needs a username and filename")
	}
	in := []map[string]any{{"filename": filename, "size": size}}
	var out struct {
		Failed []json.RawMessage `json:"failed"`
	}
	// Older slskd answers with a bare array or an empty body; decode tolerantly.
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodPost, "/transfers/downloads/"+url.PathEscape(username), in, &raw); err != nil {
		return err
	}
	if len(raw) > 0 && raw[0] == '{' {
		if err := json.Unmarshal(raw, &out); err != nil {
			return fmt.Errorf("slskd: decode enqueue answer: %w (body: %s)", err, excerpt(raw))
		}
		if len(out.Failed) > 0 {
			return fmt.Errorf("slskd: enqueue of %q from %s failed: %s", filename, username, excerpt(out.Failed[0]))
		}
	}
	return nil
}

type transferJSON struct {
	ID               string `json:"id"`
	Username         string `json:"username"`
	Filename         string `json:"filename"`
	State            string `json:"state"`
	Size             int64  `json:"size"`
	BytesTransferred int64  `json:"bytesTransferred"`
	Exception        string `json:"exception"`
}

type transfersAnswer struct {
	Directories []struct {
		Files []transferJSON `json:"files"`
	} `json:"directories"`
	Downloads []transferJSON `json:"downloads"`
}

// GetDownload returns the transfer for filename from username, or ErrNotFound when slskd has none.
func (c *Client) GetDownload(ctx context.Context, username, filename string) (Transfer, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/transfers/downloads/"+url.PathEscape(username), nil, &raw); err != nil {
		return Transfer{}, err
	}
	var all []transferJSON
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &all); err != nil {
			return Transfer{}, fmt.Errorf("slskd: decode transfers: %w (body: %s)", err, excerpt(raw))
		}
	} else if len(trimmed) > 0 {
		var ans transfersAnswer
		if err := json.Unmarshal(trimmed, &ans); err != nil {
			return Transfer{}, fmt.Errorf("slskd: decode transfers: %w (body: %s)", err, excerpt(raw))
		}
		for _, d := range ans.Directories {
			all = append(all, d.Files...)
		}
		all = append(all, ans.Downloads...)
	}
	for _, t := range all {
		if t.Filename == filename {
			if t.Username == "" {
				t.Username = username
			}
			return toTransfer(t), nil
		}
	}
	return Transfer{}, fmt.Errorf("%w: no download of %q from %s", ErrNotFound, filename, username)
}

func toTransfer(t transferJSON) Transfer {
	return Transfer{
		ID: t.ID, Username: t.Username, Filename: t.Filename, State: t.State, Phase: phaseOf(t.State),
		Size: t.Size, BytesTransferred: t.BytesTransferred, Exception: t.Exception,
	}
}

// phaseOf: only "Completed, Succeeded" is success. Any other Completed state (Errored, Rejected,
// TimedOut, Cancelled, Aborted...) is a failure; everything else is still pending.
func phaseOf(state string) Phase {
	if !strings.Contains(state, "Completed") {
		return PhasePending
	}
	if strings.Contains(state, "Succeeded") {
		return PhaseSucceeded
	}
	return PhaseFailed
}

// CancelDownload cancels (and removes) a transfer by id.
func (c *Client) CancelDownload(ctx context.Context, username, id string) error {
	return c.do(ctx, http.MethodDelete, "/transfers/downloads/"+url.PathEscape(username)+"/"+url.PathEscape(id), nil, nil)
}

// WaitDownload polls until the transfer succeeds or fails. It cancels the transfer and returns an
// error if bytes do not advance for stall, or if ctx ends first. A failed transfer returns the
// Transfer with a non-nil error so the caller cannot mistake it for complete.
func (c *Client) WaitDownload(ctx context.Context, username, filename string, stall time.Duration) (Transfer, error) {
	last := int64(-1)
	lastMove := time.Now()
	for {
		t, err := c.GetDownload(ctx, username, filename)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return Transfer{}, err
		}
		if err == nil {
			switch t.Phase {
			case PhaseSucceeded:
				return t, nil
			case PhaseFailed:
				return t, fmt.Errorf("slskd: download of %q from %s failed: %s %s", filename, username, t.State, t.Exception)
			}
			if t.BytesTransferred != last {
				last = t.BytesTransferred
				lastMove = time.Now()
			}
			if stall > 0 && time.Since(lastMove) > stall {
				if t.ID != "" {
					if cerr := c.CancelDownload(context.WithoutCancel(ctx), username, t.ID); cerr != nil {
						return t, fmt.Errorf("slskd: download of %q stalled; cancel also failed: %w", filename, cerr)
					}
				}
				return t, fmt.Errorf("slskd: download of %q from %s stalled (state %s)", filename, username, t.State)
			}
		} else if stall > 0 && time.Since(lastMove) > stall {
			return Transfer{}, fmt.Errorf("slskd: download of %q from %s never appeared: %w", filename, username, err)
		}
		if err := c.sleep(ctx); err != nil {
			return Transfer{}, err
		}
	}
}
