package notify

import (
	"context"
	"testing"
	"time"
)

var musicEngineEvents = []string{
	EventMusicEnginePlaylistReadFailed, EventMusicEngineItemPastDeadline, EventMusicEngineLaneUnavailable,
	EventMusicEnginePausedForSpace, EventMusicEngineQuarantineFull, EventMusicEngineDailyDigest,
}

// TestCatalogFormat wants a sample payload for every event.
func init() {
	for _, k := range musicEngineEvents {
		samples[k] = map[string]any{"subject": "Music engine paused", "body": "Free space on /mnt/nas/music is 90 GB, below the 100 GB floor.", "item_id": int64(7)}
	}
}

// Each engine alert is a needs-you event on by default with the retry policy; the events
// that existed before keep the default (nil policy, so 4 attempts).
func TestMusicEngineEventsCarryRetryPolicy(t *testing.T) {
	for _, k := range musicEngineEvents {
		d := mustLookup(t, k)
		if d.Retry == nil || d.Group != GroupNeedsYou || !d.DefaultOn || d.Topic != "" {
			t.Errorf("%s: %+v", k, d)
		}
	}
	for _, k := range []string{"release.grabbed", "request.created", EventImportStuck, EventHealthProblem} {
		if mustLookup(t, k).Retry != nil {
			t.Errorf("%s should keep the default retry schedule", k)
		}
	}
}

func TestRetryPolicyDelayDoublesThenCaps(t *testing.T) {
	p := MusicEngineRetry
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour}
	for i, w := range want {
		if got := p.Delay(i + 1); got != w {
			t.Errorf("Delay(%d) = %s, want %s", i+1, got, w)
		}
	}
	if got := p.Delay(500); got != time.Hour {
		t.Errorf("Delay(500) = %s, want the 1h cap", got)
	}
}

func TestMusicEngineFormat(t *testing.T) {
	d := mustLookup(t, EventMusicEnginePausedForSpace)
	m, ok := d.Format(map[string]any{"subject": "Paused", "body": "low space"})
	if !ok || m.Title != "Paused" || m.Body != "low space" || m.Link != musicEngineLink {
		t.Errorf("Format = %+v, %v", m, ok)
	}
	if m, ok := d.Format(map[string]any{"body": "only a body"}); !ok || m.Title != "Music engine paused" {
		t.Errorf("a missing subject should fall back to the event's title: %+v, %v", m, ok)
	}
	if _, ok := d.Format(map[string]any{}); ok {
		t.Error("an empty payload should send nothing")
	}
}

// A delivered engine alert is not queued again after a restart: a fresh Service over the
// same database sees the dedupe key, and so does a second emit in the same run.
func TestMusicEngineAlertNotRequeuedAfterRestart(t *testing.T) {
	s, rec, _ := newTestService(t)
	ctx := context.Background()
	mustCreate(t, s, Connection{Name: "tg", URL: "ntfy://tg", Events: sub(EventMusicEngineItemPastDeadline), Enabled: true})
	data := map[string]any{"subject": "Item 7 is stuck", "body": "in searching"}
	key := "mus-item:7:searching:100"
	if n, err := s.EmitOnce(ctx, EventMusicEngineItemPastDeadline, key, data); err != nil || n != 1 {
		t.Fatalf("first emit = %d, %v", n, err)
	}
	drainNow(t, s)
	if rec.count() != 1 {
		t.Fatalf("sends = %d, want 1", rec.count())
	}
	fresh := NewService(s.db, s.bus, s.log)
	fresh.SetTransport(rec.send)
	for i := 0; i < 2; i++ {
		if n, err := fresh.EmitOnce(ctx, EventMusicEngineItemPastDeadline, key, data); err != nil || n != 0 {
			t.Fatalf("emit %d after restart = %d, %v; want 0 queued", i, n, err)
		}
	}
	drainNow(t, fresh)
	if rec.count() != 1 {
		t.Errorf("sends after restart = %d, want still 1", rec.count())
	}
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM notification_deliveries WHERE dedupe_key = ?`, key).Scan(&rows); err != nil || rows != 1 {
		t.Errorf("rows for the key = %d, %v; want 1", rows, err)
	}
}
