package musicengine

import (
	"context"
	"sync"
	"time"
)

// Heartbeat records when the engine's scheduler last completed a tick without error, for
// the watchdog that runs on the Beast host outside the app (R30, KTD13). The watchdog
// reads it through U13's heartbeat route, which returns only this timestamp; a stale or
// unreachable answer is its cue to message Blair.
//
// It is held in memory on purpose: after a restart the value is zero until the first
// tick succeeds, which the watchdog reads as stale, and that is correct, since the engine
// has not yet shown it is working.
type Heartbeat struct {
	mu   sync.Mutex
	last time.Time
	now  func() time.Time
}

// NewHeartbeat builds a heartbeat; now defaults to time.Now.
func NewHeartbeat(now func() time.Time) *Heartbeat {
	if now == nil {
		now = time.Now
	}
	return &Heartbeat{now: now}
}

// Beat records a successful tick now.
func (h *Heartbeat) Beat() {
	h.mu.Lock()
	h.last = h.now()
	h.mu.Unlock()
}

// Last is the time of the last successful tick, zero if there has been none.
func (h *Heartbeat) Last() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last
}

// Stale reports whether the last successful tick is older than maxAge, or never happened.
func (h *Heartbeat) Stale(maxAge time.Duration) bool {
	last := h.Last()
	return last.IsZero() || h.now().Sub(last) > maxAge
}

// HeartbeatStatus is the one field the heartbeat route exposes.
type HeartbeatStatus struct {
	LastTickAt int64 `json:"last_tick_at"` // unix seconds, 0 = no successful tick yet
}

// Status is what the route returns.
func (h *Heartbeat) Status() HeartbeatStatus {
	last := h.Last()
	if last.IsZero() {
		return HeartbeatStatus{}
	}
	return HeartbeatStatus{LastTickAt: last.Unix()}
}

// Ticker is the part of the engine the scheduler runs.
type Ticker interface {
	Tick(ctx context.Context) error
}

// Tick runs one engine tick and beats only if it returned no error: a tick that failed
// (the database unreadable, a notifier refusing) is not a sign of life.
func (h *Heartbeat) Tick(ctx context.Context, t Ticker) error {
	if err := t.Tick(ctx); err != nil {
		return err
	}
	h.Beat()
	return nil
}
