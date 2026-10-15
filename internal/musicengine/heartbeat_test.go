package musicengine

import (
	"context"
	"errors"
	"testing"
	"time"
)

type hbFakeTicker struct{ err error }

func (f hbFakeTicker) Tick(context.Context) error { return f.err }

func TestHeartbeatRecordsOnlySuccessfulTicks(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	h := NewHeartbeat(clk.Now)
	if !h.Last().IsZero() || !h.Stale(time.Hour) || h.Status().LastTickAt != 0 {
		t.Fatal("a heartbeat that never beat is stale and reports no time")
	}

	if err := h.Tick(context.Background(), hbFakeTicker{}); err != nil {
		t.Fatal(err)
	}
	first := clk.Now()
	if !h.Last().Equal(first) || h.Status().LastTickAt != first.Unix() || h.Stale(10*time.Minute) {
		t.Fatalf("after a good tick: last=%s status=%+v", h.Last(), h.Status())
	}

	// A failing tick does not refresh it, and the error comes back.
	clk.Advance(15 * time.Minute)
	boom := errors.New("database is locked")
	if err := h.Tick(context.Background(), hbFakeTicker{err: boom}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the tick's error", err)
	}
	if !h.Last().Equal(first) {
		t.Fatalf("last moved to %s on a failed tick", h.Last())
	}
	if !h.Stale(10 * time.Minute) {
		t.Fatal("15 minutes without a good tick is stale at a 10 minute limit")
	}

	// A real engine tick on an empty database succeeds and beats.
	v := newEnv(t)
	hb := NewHeartbeat(v.clock.Now)
	if err := hb.Tick(context.Background(), v.e); err != nil || hb.Last().IsZero() {
		t.Fatalf("engine tick: err=%v last=%s", err, hb.Last())
	}
}
