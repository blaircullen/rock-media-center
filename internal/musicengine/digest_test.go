package musicengine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/tristenlammi/arrmada/internal/eventbus"
	"github.com/tristenlammi/arrmada/internal/notify"
)

// digestSwap inserts a completed swap closed at the given time for a library track.
func (v *env) digestSwap(artist, title string, closedAt time.Time, undone bool) int64 {
	v.t.Helper()
	res, err := v.db.Exec(
		`INSERT INTO mus_library_tracks (plex_rating_key, path, title, artist, created_at) VALUES (?, ?, ?, ?, ?)`,
		fmt.Sprintf("rk-%s-%d", title, closedAt.UnixNano()), "/music/"+title+".flac", title, artist, v.clock.Now().Unix())
	if err != nil {
		v.t.Fatal(err)
	}
	track, _ := res.LastInsertId()
	undoneAt := int64(0)
	if undone {
		undoneAt = closedAt.Unix() + 60
	}
	res, err = v.db.Exec(
		`INSERT INTO mus_swap_journal (kind, track_id, new_path, phase, undone_at, started_at, phase_at, closed_at)
		 VALUES ('swap', ?, ?, 5, ?, ?, ?, ?)`,
		track, "/music/"+title+".flac", undoneAt, closedAt.Unix()-60, closedAt.Unix(), closedAt.Unix())
	if err != nil {
		v.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// digestDecision gives a pinned item an open decision created at the given time.
func (v *env) digestDecision(artist, title, reason string, created time.Time) int64 {
	v.t.Helper()
	rec := v.seedRecording("rec-"+title, title, artist, "", "")
	item := v.seedItem(KindRequest, StateWaitingBlair, rec)
	res, err := v.db.Exec(`INSERT INTO mus_decisions (item_id, kind, reason, options, created_at) VALUES (?, 'no_genuine_copy', ?, '[]', ?)`,
		item, reason, created.Unix())
	if err != nil {
		v.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// The digest lists the day's swaps with their undo link, the new decisions, and rolls
// everything waiting more than 3 days into one line. An undone swap and a swap from
// another day are left out.
func TestDigestListsSwapsDecisionsAndRollsUpOldWaits(t *testing.T) {
	v := newEnv(t)
	now := v.clock.Now()
	swapA := v.digestSwap("Robyn", "Dancing On My Own", now.Add(-3*time.Hour), false)
	v.digestSwap("Prince", "Kiss", now.Add(-2*time.Hour), true)    // undone
	v.digestSwap("Cher", "Believe", now.Add(-30*time.Hour), false) // yesterday's
	v.digestDecision("Muse", "Hysteria", "no genuine copy found", now.Add(-2*time.Hour))
	for i := 0; i < 7; i++ {
		v.digestDecision("Old Artist", fmt.Sprintf("Old Song %d", i), "waiting", now.Add(-time.Duration(4+i)*24*time.Hour))
	}
	// Two days old: in an earlier digest, not old enough for the long-wait line.
	v.digestDecision("Mid", "Midway", "waiting", now.Add(-50*time.Hour))

	g := NewDigest(v.db, v.notifier, v.clock.Now)
	d, err := g.Build(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Swaps) != 1 || d.Swaps[0].SwapID != swapA || d.Swaps[0].Label != "Robyn - Dancing On My Own" {
		t.Fatalf("swaps = %+v, want only the live swap of the last day", d.Swaps)
	}
	if len(d.NewDecisions) != 1 || d.NewDecisions[0].Label != "Muse - Hysteria" {
		t.Fatalf("new decisions = %+v", d.NewDecisions)
	}
	if len(d.LongWaiting) != 7 {
		t.Fatalf("long waiting = %d, want 7", len(d.LongWaiting))
	}

	subject, body := d.Text()
	if subject == "" {
		t.Error("empty subject")
	}
	wantUndo := fmt.Sprintf("swap %d, undo at %s%d", swapA, digestUndoLink, swapA)
	if !strings.Contains(body, "Robyn - Dancing On My Own: "+wantUndo) {
		t.Errorf("body lacks the swap with its undo link:\n%s", body)
	}
	if !strings.Contains(body, "Muse - Hysteria: no genuine copy found") {
		t.Errorf("body lacks the new decision:\n%s", body)
	}
	if n := strings.Count(body, "Waiting on you more than 3 days"); n != 1 {
		t.Errorf("long-wait lines = %d, want exactly one:\n%s", n, body)
	}
	if !strings.Contains(body, "7 songs") || !strings.Contains(body, "and 2 more") {
		t.Errorf("long-wait line should count 7 and name 5:\n%s", body)
	}
	if strings.Contains(body, "Midway") || strings.Contains(body, "Believe") || strings.Contains(body, "Kiss") {
		t.Errorf("body includes something it should leave out:\n%s", body)
	}
}

func TestDigestSendsOncePerDayAndNothingWhenQuiet(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()
	g := NewDigest(v.db, v.notifier, v.clock.Now)
	if sent, err := g.Send(ctx); err != nil || sent || v.notifier.count(AlertDailyDigest) != 0 {
		t.Fatalf("a quiet day: sent=%v err=%v alerts=%d, want nothing", sent, err, v.notifier.count(AlertDailyDigest))
	}
	v.digestSwap("A", "One", v.clock.Now().Add(-time.Hour), false)

	v.notifier.err = errors.New("queue unavailable")
	if sent, err := g.Send(ctx); err == nil || sent {
		t.Fatalf("a refused digest must be an error, not a send: sent=%v err=%v", sent, err)
	}
	v.notifier.err = nil
	for i := 0; i < 3; i++ {
		sent, err := g.Send(ctx)
		if err != nil || sent != (i == 0) {
			t.Fatalf("send %d: sent=%v err=%v", i, sent, err)
		}
	}
	if n := v.notifier.count(AlertDailyDigest); n != 1 {
		t.Fatalf("digests queued = %d, want 1", n)
	}
	if k := v.notifier.alerts[len(v.notifier.alerts)-1].Key; k != "mus-digest:2026-10-14" {
		t.Errorf("dedupe key = %q, want the Eastern date", k)
	}

	// A fresh sender (restart) hands the same key to the queue, which dedupes it.
	again := NewDigest(v.db, v.notifier, v.clock.Now)
	if _, err := again.Send(ctx); err != nil {
		t.Fatal(err)
	}
	last := v.notifier.alerts[len(v.notifier.alerts)-1].Key
	if last != "mus-digest:2026-10-14" {
		t.Errorf("key after restart = %q, want the same", last)
	}
}

func TestCheckLanesAlertsAfterADayWithAStableKey(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()
	now := v.clock.Now()
	for lane, since := range map[string]time.Duration{"usenet": 25 * time.Hour, "torrent": 3 * time.Hour} {
		if _, err := v.db.Exec(`INSERT INTO mus_lane_status (lane, status, reason, unavailable_since) VALUES (?, 'unavailable', 'key rejected', ?)`,
			lane, now.Add(-since).Unix()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := v.db.Exec(`INSERT INTO mus_lane_status (lane, status) VALUES ('youtube', 'available')`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := CheckLanes(ctx, v.db, v.notifier, now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]int{}
	for _, a := range v.notifier.alerts {
		if a.Kind == AlertLaneUnavailable {
			seen[a.Key]++
		}
	}
	if len(seen) != 1 {
		t.Fatalf("alert keys = %v, want one key (usenet only)", seen)
	}
	for k := range seen {
		if !strings.HasPrefix(k, "mus-lane:usenet:") {
			t.Errorf("key %q should name the lane and when it went down", k)
		}
	}
	v.notifier.err = errors.New("down")
	if err := CheckLanes(ctx, v.db, v.notifier, now); err == nil {
		t.Error("a refused lane alert must be returned so the next run retries it")
	}
}

// The same past-deadline condition queues one alert across ticks, through the real notify
// queue and not only through the engine's own bookkeeping.
func TestPastDeadlineQueuesOneAlertAcrossTicks(t *testing.T) {
	v := newEnv(t)
	svc := digestNotifyService(t, v)
	e, err := New(Config{DB: v.db, Notifier: NewNotifier(svc), Acquirer: v.acquirer, Now: v.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	item := v.seedItem(KindRequest, StateSearching, 0)
	v.clock.Advance(DeadlineSearching + time.Hour)
	for i := 0; i < 4; i++ {
		if err := e.Tick(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	if n := v.count(`SELECT COUNT(*) FROM notification_deliveries WHERE event_key = ?`, notify.EventMusicEngineItemPastDeadline); n != 1 {
		t.Fatalf("queued alerts = %d, want 1 for item %d", n, item)
	}

	// A restart: a new engine over the same database does not queue it again.
	e2, err := New(Config{DB: v.db, Notifier: NewNotifier(svc), Acquirer: v.acquirer, Now: v.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if err := e2.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := v.count(`SELECT COUNT(*) FROM notification_deliveries WHERE event_key = ?`, notify.EventMusicEngineItemPastDeadline); n != 1 {
		t.Fatalf("queued alerts after restart = %d, want still 1", n)
	}
}

// Every alert kind maps to an event that exists in the notify catalogue, so no alert dies
// as "unknown alert event".
func TestEveryAlertKindHasACatalogueEvent(t *testing.T) {
	v := newEnv(t)
	svc := digestNotifyService(t, v)
	n := NewNotifier(svc)
	kinds := []AlertKind{AlertPlaylistReadFailed, AlertItemPastDeadline, AlertLaneUnavailable, AlertPausedForSpace, AlertQuarantineFull, AlertDailyDigest}
	if len(kinds) != len(alertEvents) {
		t.Fatalf("alertEvents has %d entries, test lists %d", len(alertEvents), len(kinds))
	}
	for _, k := range kinds {
		def, ok := notify.Lookup(alertEvents[k])
		if !ok || def.Retry == nil {
			t.Errorf("%s -> %q: in catalogue=%v with retry policy=%v", k, alertEvents[k], ok, ok && def.Retry != nil)
		}
		if err := n.Alert(context.Background(), Alert{Kind: k, Key: "k-" + string(k), Subject: "s", Body: "b"}); err != nil {
			t.Errorf("%s: %v", k, err)
		}
	}
	if err := n.Alert(context.Background(), Alert{Kind: "nonsense", Key: "x", Subject: "s"}); err == nil {
		t.Error("an unknown kind must be an error, not a silent drop")
	}
}

func digestNotifyService(t *testing.T, v *env) *notify.Service {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := notify.NewService(v.db, eventbus.New(log), log)
	svc.SetTransport(func(context.Context, string, string, string) error { return nil })
	if _, err := svc.Create(context.Background(), notify.Connection{
		Name: "tg", URL: "ntfy://tg", Enabled: true,
		Events: []string{
			notify.EventMusicEnginePlaylistReadFailed, notify.EventMusicEngineItemPastDeadline, notify.EventMusicEngineLaneUnavailable,
			notify.EventMusicEnginePausedForSpace, notify.EventMusicEngineQuarantineFull, notify.EventMusicEngineDailyDigest,
		},
	}); err != nil {
		t.Fatal(err)
	}
	return svc
}
