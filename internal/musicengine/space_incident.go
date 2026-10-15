package musicengine

import "context"

// Incident names stored in mus_alert_incidents.
const (
	incidentSpacePause     = "space-pause"
	incidentQuarantineFull = "quarantine-full"
)

// incidentStart returns the unix time the named incident began, creating it at now when none
// is open. Alert dedupe keys embed this value, so reusing the stored one across a restart keeps
// one outage one alert. When the database cannot be used the fallback is now, and the failure
// is logged: the alert may repeat after a restart, but it is never lost.
func (s *Space) incidentStart(ctx context.Context, name string, now int64) int64 {
	var started int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO mus_alert_incidents (name, started_at) VALUES (?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		        started_at = CASE WHEN mus_alert_incidents.cleared_at <> 0 THEN excluded.started_at ELSE mus_alert_incidents.started_at END,
		        cleared_at = 0
		 RETURNING started_at`, name, now).Scan(&started)
	if err != nil {
		s.log.Warn("musicengine: could not persist alert incident, the alert key may repeat after a restart", "incident", name, "err", err)
		return now
	}
	delete(s.incidentCleared, name)
	return started
}

// incidentClear closes the named incident once the condition has recovered. The database is
// written once per recovery, not on every evaluation.
func (s *Space) incidentClear(ctx context.Context, name string, now int64) {
	if s.incidentCleared == nil {
		s.incidentCleared = map[string]bool{}
	}
	if s.incidentCleared[name] {
		return
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE mus_alert_incidents SET cleared_at = ? WHERE name = ? AND cleared_at = 0`, now, name); err != nil {
		s.log.Warn("musicengine: could not close alert incident, it will be retried", "incident", name, "err", err)
		return
	}
	s.incidentCleared[name] = true
}
