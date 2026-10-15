package notify

import "time"

// The lossless music engine's alerts (internal/musicengine, plan KTD11). No bus topic: the
// engine's notifier calls EmitOnce with a dedupe key per item and state, so each alert is
// queued once per connection and survives a restart. Every one carries a RetryPolicy:
// the engine's alerts are retried with backoff capped at an hour until delivered, where
// other events give up after 4 attempts.
const (
	EventMusicEnginePlaylistReadFailed = "musicengine.playlist_read_failed"
	EventMusicEngineItemPastDeadline   = "musicengine.item_past_deadline"
	EventMusicEngineLaneUnavailable    = "musicengine.lane_unavailable"
	EventMusicEnginePausedForSpace     = "musicengine.paused_for_space"
	EventMusicEngineQuarantineFull     = "musicengine.quarantine_full"
	EventMusicEngineDailyDigest        = "musicengine.daily_digest"
)

// MusicEngineRetry is the engine alerts' retry schedule: 1 minute doubling to 1 hour,
// then every hour until the target answers.
var MusicEngineRetry = RetryPolicy{Base: time.Minute, Max: time.Hour}

const musicEngineLink = "/music-engine"

func init() {
	for _, d := range []struct{ key, label, hint, fallback string }{
		{EventMusicEnginePlaylistReadFailed, "Music: playlist could not be read", "The watched Spotify playlist could not be read completely, so nothing was changed.", "Spotify playlist could not be read"},
		{EventMusicEngineItemPastDeadline, "Music: a song is stuck", "A song stayed in one step past its deadline.", "A music item is stuck"},
		{EventMusicEngineLaneUnavailable, "Music: a source has been down", "A download source (Usenet, torrents, Soulseek or YouTube) has been unavailable for more than a day.", "A music source is unavailable"},
		{EventMusicEnginePausedForSpace, "Music: paused for space or storage", "The music engine paused because disk space is low or a volume cannot be read. Nothing is deleted.", "Music engine paused"},
		{EventMusicEngineQuarantineFull, "Music: quarantine is full", "Replaced versions have used the quarantine share, so the upgrade sweep is paused until you clear old ones.", "Music quarantine is full"},
		{EventMusicEngineDailyDigest, "Music: daily summary", "Swaps (with undo), new decisions waiting on you, and songs waiting more than 3 days.", "Music daily summary"},
	} {
		fallback := d.fallback
		Register(EventDef{
			Key: d.key, Group: GroupNeedsYou, DefaultOn: true,
			Label: d.label, Hint: d.hint,
			Retry:  &MusicEngineRetry,
			Format: func(data map[string]any) (Message, bool) { return formatMusicEngine(fallback, data) },
		})
	}
}

// formatMusicEngine writes an engine alert. Payload: subject and body, both written by the
// engine. An alert with neither says nothing.
func formatMusicEngine(fallback string, d map[string]any) (Message, bool) {
	subject, body := str(d, "subject"), str(d, "body")
	if subject == "" && body == "" {
		return Message{}, false
	}
	if subject == "" {
		subject = fallback
	}
	return Message{Title: subject, Body: body, Link: musicEngineLink}, true
}
