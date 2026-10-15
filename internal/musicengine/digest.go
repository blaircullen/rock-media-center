package musicengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// The daily digest (R25, R31) and the alert producers that are not a state-machine event:
// a lane down for more than a day, and the notifier that hands every alert to the notify
// queue (KTD11).

const (
	// DigestLongWait is how long a decision waits before it is rolled into the digest's
	// single "waiting more than 3 days" line.
	DigestLongWait = 3 * 24 * time.Hour
	// LaneAlertAfter is how long a lane stays unavailable before Blair is told (U12 step 4).
	LaneAlertAfter = 24 * time.Hour

	digestWindow    = 24 * time.Hour
	digestNameLimit = 5
	// digestUndoLink is where the status page undoes a swap; the digest names it per swap.
	digestUndoLink = "/music-engine?tab=swaps&undo="
)

// Alert kinds raised by this unit, beside the two the core raises.
const (
	AlertLaneUnavailable AlertKind = "lane_unavailable"
	AlertPausedForSpace  AlertKind = "paused_for_space"
	AlertQuarantineFull  AlertKind = "quarantine_full"
	AlertDailyDigest     AlertKind = "daily_digest"
)

// DigestSwap is one swap of the last day.
type DigestSwap struct {
	SwapID int64
	Label  string
	At     int64
}

// DigestDecision is one decision waiting on Blair.
type DigestDecision struct {
	DecisionID int64
	ItemID     int64
	Kind       string
	Label      string
	Reason     string
	CreatedAt  int64
}

// Digest is one day's summary.
type Digest struct {
	Since, Until time.Time
	Swaps        []DigestSwap
	NewDecisions []DigestDecision
	// LongWaiting are the decisions open for more than DigestLongWait.
	LongWaiting []DigestDecision
}

// Empty reports whether there is nothing to say; the engine sends no digest then.
func (d Digest) Empty() bool {
	return len(d.Swaps) == 0 && len(d.NewDecisions) == 0 && len(d.LongWaiting) == 0
}

// Text writes the digest as a subject and a plain-text body.
func (d Digest) Text() (subject, body string) {
	var parts []string
	var b strings.Builder
	if len(d.Swaps) > 0 {
		fmt.Fprintf(&b, "Swaps (%d)", len(d.Swaps))
		for _, s := range d.Swaps {
			fmt.Fprintf(&b, "\n- %s: swap %d, undo at %s%d", s.Label, s.SwapID, digestUndoLink, s.SwapID)
		}
		parts = append(parts, b.String())
		b.Reset()
	}
	if len(d.NewDecisions) > 0 {
		fmt.Fprintf(&b, "New decisions (%d)", len(d.NewDecisions))
		for _, x := range d.NewDecisions {
			fmt.Fprintf(&b, "\n- %s: %s", x.Label, x.Reason)
		}
		parts = append(parts, b.String())
		b.Reset()
	}
	if n := len(d.LongWaiting); n > 0 {
		names := make([]string, 0, digestNameLimit)
		for i, x := range d.LongWaiting {
			if i == digestNameLimit {
				break
			}
			names = append(names, x.Label)
		}
		line := fmt.Sprintf("Waiting on you more than 3 days: %d song%s (%s", n, digestPlural(n), strings.Join(names, "; "))
		if n > digestNameLimit {
			line += fmt.Sprintf("; and %d more", n-digestNameLimit)
		}
		parts = append(parts, line+")")
	}
	return "Music engine daily summary", strings.Join(parts, "\n\n")
}

func digestPlural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// DigestSender builds and sends the daily digest.
type DigestSender struct {
	db       *sql.DB
	notifier Notifier
	now      func() time.Time
	loc      *time.Location

	mu      sync.Mutex
	sentDay string
}

// NewDigest builds the sender. Days are cut in Eastern time (the house clock); if the zone
// data is missing the digest falls back to UTC rather than failing.
func NewDigest(db *sql.DB, n Notifier, now func() time.Time) *DigestSender {
	if now == nil {
		now = time.Now
	}
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		slog.Warn("musicengine: America/New_York zone data missing, cutting digest days in UTC", "err", err)
		loc = time.UTC
	}
	return &DigestSender{db: db, notifier: n, now: now, loc: loc}
}

// Build reads the digest for the day ending at until.
func (g *DigestSender) Build(ctx context.Context, until time.Time) (Digest, error) {
	since := until.Add(-digestWindow)
	d := Digest{Since: since, Until: until}

	rows, err := g.db.QueryContext(ctx,
		`SELECT j.id, j.closed_at, COALESCE(t.artist, ''), COALESCE(t.title, ''), j.new_path
		 FROM mus_swap_journal j LEFT JOIN mus_library_tracks t ON t.id = j.track_id
		 WHERE j.kind = 'swap' AND j.phase = 5 AND j.closed_at >= ? AND j.closed_at < ? AND j.undone_at = 0
		 ORDER BY j.id`, since.Unix(), until.Unix())
	if err != nil {
		return d, fmt.Errorf("listing the day's swaps: %w", err)
	}
	for rows.Next() {
		var s DigestSwap
		var artist, title, path string
		if err := rows.Scan(&s.SwapID, &s.At, &artist, &title, &path); err != nil {
			rows.Close()
			return d, fmt.Errorf("scanning a swap: %w", err)
		}
		s.Label = digestLabel(artist, title, path)
		d.Swaps = append(d.Swaps, s)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return d, err
	}
	rows.Close()

	rows, err = g.db.QueryContext(ctx,
		`SELECT d.id, d.item_id, d.kind, d.reason, d.created_at,
		        COALESCE(NULLIF(r.artist, ''), (SELECT artist FROM mus_playlist_entries WHERE item_id = d.item_id ORDER BY id LIMIT 1), ''),
		        COALESCE(NULLIF(r.title, ''),  (SELECT title  FROM mus_playlist_entries WHERE item_id = d.item_id ORDER BY id LIMIT 1), '')
		 FROM mus_decisions d
		 JOIN mus_items i ON i.id = d.item_id
		 LEFT JOIN mus_recordings r ON r.id = i.recording_id
		 WHERE d.answered_at = 0 AND d.created_at < ?
		 ORDER BY d.created_at, d.id`, until.Unix())
	if err != nil {
		return d, fmt.Errorf("listing open decisions: %w", err)
	}
	defer rows.Close()
	cutoff := until.Add(-DigestLongWait).Unix()
	for rows.Next() {
		var x DigestDecision
		var artist, title string
		if err := rows.Scan(&x.DecisionID, &x.ItemID, &x.Kind, &x.Reason, &x.CreatedAt, &artist, &title); err != nil {
			return d, fmt.Errorf("scanning a decision: %w", err)
		}
		x.Label = digestLabel(artist, title, fmt.Sprintf("item %d", x.ItemID))
		switch {
		case x.CreatedAt <= cutoff:
			d.LongWaiting = append(d.LongWaiting, x)
		case x.CreatedAt >= since.Unix():
			d.NewDecisions = append(d.NewDecisions, x)
		}
		// A decision between 1 and 3 days old was in an earlier digest and is not yet old
		// enough for the long-wait line: it is not repeated.
	}
	return d, rows.Err()
}

func digestLabel(artist, title, fallback string) string {
	switch {
	case artist != "" && title != "":
		return artist + " - " + title
	case title != "":
		return title
	}
	return fallback
}

// Send builds the last day's digest and queues it once. It sends nothing on a day with
// nothing to report. The dedupe key is the date, so a second call the same day, or after a
// restart, queues nothing more (the queue enforces it; sentDay only saves the work).
func (g *DigestSender) Send(ctx context.Context) (sent bool, err error) {
	now := g.now()
	day := now.In(g.loc).Format("2006-01-02")
	g.mu.Lock()
	done := g.sentDay == day
	g.mu.Unlock()
	if done {
		return false, nil
	}
	d, err := g.Build(ctx, now)
	if err != nil {
		return false, err
	}
	if d.Empty() {
		return false, nil
	}
	subject, body := d.Text()
	if err := g.notifier.Alert(ctx, Alert{Kind: AlertDailyDigest, Key: "mus-digest:" + day, Subject: subject, Body: body}); err != nil {
		return false, fmt.Errorf("queueing the daily digest: %w", err)
	}
	g.mu.Lock()
	g.sentDay = day
	g.mu.Unlock()
	return true, nil
}

// CheckLanes alerts for each lane that has been unavailable for more than LaneAlertAfter.
// The dedupe key names the lane and when it went down, so one outage is one alert however
// often this runs. A refused alert is returned and tried again on the next run.
func CheckLanes(ctx context.Context, db *sql.DB, n Notifier, now time.Time) error {
	rows, err := db.QueryContext(ctx,
		`SELECT lane, reason, unavailable_since FROM mus_lane_status
		 WHERE status = 'unavailable' AND unavailable_since > 0 AND unavailable_since <= ?
		 ORDER BY lane`, now.Add(-LaneAlertAfter).Unix())
	if err != nil {
		return fmt.Errorf("listing unavailable lanes: %w", err)
	}
	type down struct {
		lane, reason string
		since        int64
	}
	var lanes []down
	for rows.Next() {
		var l down
		if err := rows.Scan(&l.lane, &l.reason, &l.since); err != nil {
			rows.Close()
			return fmt.Errorf("scanning a lane: %w", err)
		}
		lanes = append(lanes, l)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	var errs []error
	for _, l := range lanes {
		err := n.Alert(ctx, Alert{
			Kind:    AlertLaneUnavailable,
			Key:     fmt.Sprintf("mus-lane:%s:%d", l.lane, l.since),
			Subject: fmt.Sprintf("Music source %s has been unavailable for over a day", l.lane),
			Body: fmt.Sprintf("The %s source has been unavailable since %s. Why: %s. Songs it could have found stay in the queue and are not marked as having no lossless copy.",
				l.lane, time.Unix(l.since, 0).UTC().Format(time.RFC3339), l.reason),
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("alerting for lane %s: %w", l.lane, err))
		}
	}
	return errors.Join(errs...)
}

// AlertEmitter is the notify service's EmitOnce, which *notify.Service satisfies.
type AlertEmitter interface {
	EmitOnce(ctx context.Context, key, dedupe string, data map[string]any) (int, error)
}

// alertEvents maps each alert kind to its event in internal/notify/catalog_musicengine.go.
var alertEvents = map[AlertKind]string{
	AlertPlaylistReadFailed: "musicengine.playlist_read_failed",
	AlertItemPastDeadline:   "musicengine.item_past_deadline",
	AlertLaneUnavailable:    "musicengine.lane_unavailable",
	AlertPausedForSpace:     "musicengine.paused_for_space",
	AlertQuarantineFull:     "musicengine.quarantine_full",
	AlertDailyDigest:        "musicengine.daily_digest",
}

type queueNotifier struct{ emit AlertEmitter }

// NewNotifier is the Notifier over the notify queue (KTD11): each alert is emitted with
// its key as the dedupe key, so it is queued once per connection however often the
// engine raises it, and the event's retry policy keeps it going until delivered. An error
// means it was not queued.
func NewNotifier(emit AlertEmitter) Notifier { return queueNotifier{emit: emit} }

func (q queueNotifier) Alert(ctx context.Context, a Alert) error {
	event, ok := alertEvents[a.Kind]
	if !ok {
		return fmt.Errorf("musicengine: no notify event for alert kind %q", a.Kind)
	}
	data := map[string]any{"subject": a.Subject, "body": a.Body}
	if a.ItemID != 0 {
		data["item_id"] = a.ItemID
	}
	if _, err := q.emit.EmitOnce(ctx, event, a.Key, data); err != nil {
		return fmt.Errorf("queueing %s alert %q: %w", a.Kind, a.Key, err)
	}
	return nil
}
