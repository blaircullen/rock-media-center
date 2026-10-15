package musicengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/tristenlammi/arrmada/internal/audiocheck"
	"github.com/tristenlammi/arrmada/internal/plex"
	"github.com/tristenlammi/arrmada/internal/spotify"
	"github.com/tristenlammi/arrmada/internal/store"
)

// This file declares everything the engine core depends on, so tests use fakes, and the
// seams later units implement (U9 acquisition, U10 placement, U12 space/alerts/decisions).
// Every seam has a default here, so the engine runs with them absent.

// ---- building blocks the core calls ----

// SpotifyReader is internal/spotify's playlist read. It returns a complete, verified read or
// an error and no entries (R5).
type SpotifyReader interface {
	ReadPlaylist(ctx context.Context, playlistID string) (*spotify.Playlist, error)
}

// PlexLibrary is the slice of internal/plex the core reads. Playlists leaves out smart
// playlists, as the real client does; the catalogue also drops any marked Smart.
type PlexLibrary interface {
	Tracks(ctx context.Context, sectionKey string) ([]plex.Track, error)
	SearchTracks(ctx context.Context, sectionKey, artist, title string) ([]plex.Track, error)
	Playlists(ctx context.Context) ([]plex.Playlist, error)
	PlaylistItems(ctx context.Context, playlistKey string) ([]plex.PlaylistItem, error)
}

// AudioChecker is internal/audiocheck's Check. It never returns an error: a failure to look
// is the Blind verdict.
type AudioChecker interface {
	Check(ctx context.Context, path string) audiocheck.Result
}

// Tags is what the library check reads from a file's metadata.
type Tags struct {
	RecordingMBID string // MUSICBRAINZ_TRACKID (Picard's name for the recording id)
	ReleaseMBID   string // MUSICBRAINZ_ALBUMID
	ISRC          string
	Title         string
	Artist        string
	Album         string
	DurationSec   float64
}

// TagReader reads a file's tags (ffprobe through internal/toolrun in production). A read
// that fails is an error, never empty Tags: an unreadable file is not an unmatched one.
type TagReader interface {
	ReadTags(ctx context.Context, path string) (Tags, error)
}

var (
	_ SpotifyReader = (*spotify.Client)(nil)
	_ PlexLibrary   = (*plex.Client)(nil)
	_ AudioChecker  = (*audiocheck.Checker)(nil)
)

// ---- seams ----

// Machine is the state machine handle the seams get, so their own data writes and the state
// change land in one transaction (KTD4): open store.WithTx on DB(), write, then call
// TransitionTx with the same tx.
type Machine interface {
	// Transition moves one item in its own transaction.
	Transition(ctx context.Context, itemID int64, to State, reason string) error
	// TransitionTx moves one item inside the caller's transaction.
	TransitionTx(ctx context.Context, tx store.Execer, itemID int64, to State, reason string) error
	DB() *sql.DB
	Now() time.Time
}

// Acquirer is implemented by U9. Step is called for an item in searching, lane_unavailable,
// downloading, checking or failed_check, when the item is due. It must be idempotent: after
// a restart the item is handed over again in the same state. The default does nothing, so
// the item waits (and its deadline alert fires).
type Acquirer interface {
	Step(ctx context.Context, m Machine, it Item) error
}

// Placer is implemented by U10 and U11. Step is called for an item in placing, swapping,
// waiting_for_playback or awaiting_plex_index. Same idempotence rule as Acquirer.
type Placer interface {
	Step(ctx context.Context, m Machine, it Item) error
}

// WorkKind says which kind of work asks the space gate for room.
type WorkKind int

const (
	// WorkIntake is a playlist request (R28: paused second).
	WorkIntake WorkKind = iota
	// WorkSweep is an upgrade-sweep item (R28: paused first).
	WorkSweep
)

// SpaceGate is implemented by U12. Allow is asked before an item starts searching, and
// again for each paused item to learn whether it may resume. A gate that cannot decide says
// no (KTD10).
type SpaceGate interface {
	Allow(ctx context.Context, kind WorkKind) (ok bool, reason string)
}

// AlertKind names an engine alert. The core raises only the first two.
type AlertKind string

const (
	AlertPlaylistReadFailed AlertKind = "playlist_read_failed"
	AlertItemPastDeadline   AlertKind = "item_past_deadline"
)

// Alert is one message for Blair. Key is the dedupe key; the sender queues each key once
// (KTD11).
type Alert struct {
	Kind    AlertKind
	Key     string
	Subject string
	Body    string
	ItemID  int64
}

// Notifier is implemented by U12 over the notify queue. A returned error means the alert was
// not queued; the core then tries again on its next run.
type Notifier interface {
	Alert(ctx context.Context, a Alert) error
}

// Decision options the core raises. U12/U13 interpret the answers.
const (
	OptionDrop         = "drop"
	OptionRetry        = "retry"       // run the same step again
	OptionUsePrefix    = "use:"        // "use:<mbid>": pin to that recording (Engine.PinToRecording)
	OptionTreatAsMatch = "treat_match" // the unsettled library file is this recording
	OptionNotAMatch    = "not_match"   // it is not; go on to searching
)

// DecisionRequest asks for Blair's decision on an item.
type DecisionRequest struct {
	ItemID  int64
	Kind    string // one of the mus_decisions kinds, e.g. "ambiguous", "blind_library"
	Reason  string
	Options []string
	Now     time.Time
}

// DecisionRaiser is implemented by U12. Raise writes with the caller's tx and is idempotent:
// a second open decision of the same kind on the same item changes nothing.
type DecisionRaiser interface {
	Raise(ctx context.Context, q store.Execer, d DecisionRequest) error
}

// ---- defaults ----

type noopAcquirer struct{}

func (noopAcquirer) Step(context.Context, Machine, Item) error { return nil }

type noopPlacer struct{}

func (noopPlacer) Step(context.Context, Machine, Item) error { return nil }

type allowAllSpace struct{}

func (allowAllSpace) Allow(context.Context, WorkKind) (bool, string) { return true, "" }

// logNotifier writes alerts to the log; with no queue wired they are at least not silent.
type logNotifier struct{ log *slog.Logger }

func (n logNotifier) Alert(_ context.Context, a Alert) error {
	n.log.Warn("musicengine alert (no notifier wired)", "kind", a.Kind, "key", a.Key, "subject", a.Subject, "body", a.Body)
	return nil
}

// tableDecisions writes the decision row itself, so waiting_blair items always have a
// decision to show even before U12's list exists.
type tableDecisions struct{}

func (tableDecisions) Raise(ctx context.Context, q store.Execer, d DecisionRequest) error {
	opts, err := json.Marshal(d.Options)
	if err != nil {
		return fmt.Errorf("encoding decision options: %w", err)
	}
	if d.Options == nil {
		opts = []byte("[]")
	}
	_, err = q.ExecContext(ctx,
		`INSERT INTO mus_decisions (item_id, kind, reason, options, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(item_id, kind) WHERE answered_at = 0 DO NOTHING`,
		d.ItemID, d.Kind, d.Reason, string(opts), d.Now.Unix())
	if err != nil {
		return fmt.Errorf("raising %s decision for item %d: %w", d.Kind, d.ItemID, err)
	}
	return nil
}
