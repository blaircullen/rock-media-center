package musicengine

import (
	"testing"
	"time"
)

func alertKeys(n *recNotifier, kind AlertKind) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, a := range n.alerts {
		if a.Kind == kind {
			out = append(out, a.Key)
		}
	}
	return out
}

// Astra #21: one unbroken outage keeps one alert key across a restart (a new Space over the
// same database, a later clock); a recovery then a new outage gets a new key.
func TestSpacePauseAlertKeySurvivesRestartUntilRecovery(t *testing.T) {
	v := newEnv(t)
	vols := &spaceFakeVolumes{}
	vols.set(120, 300) // sweep paused
	s := v.newSpace(vols)
	spaceAllow(t, s, WorkSweep)
	first := alertKeys(v.notifier, AlertPausedForSpace)
	if len(first) != 1 {
		t.Fatalf("alerts = %v, want 1", first)
	}

	v.clock.Advance(3 * time.Hour)
	s2 := v.newSpace(vols) // restart: in-memory episode is gone
	spaceAllow(t, s2, WorkSweep)
	got := alertKeys(v.notifier, AlertPausedForSpace)
	if len(got) != 2 || got[1] != first[0] {
		t.Fatalf("keys after restart = %v, want the same key %q reused", got, first[0])
	}

	// Recovery closes the incident; the next outage is a new incident with a new key.
	vols.set(400, 400)
	spaceAllow(t, s2, WorkSweep)
	v.clock.Advance(time.Hour)
	vols.set(120, 300)
	spaceAllow(t, s2, WorkSweep)
	got = alertKeys(v.notifier, AlertPausedForSpace)
	if len(got) != 3 || got[2] == first[0] {
		t.Fatalf("keys after recovery and a new outage = %v, want a new key", got)
	}
}

func TestQuarantineFullAlertKeySurvivesRestart(t *testing.T) {
	v := newEnv(t)
	vols := &spaceFakeVolumes{}
	vols.set(900, 900)
	if _, err := v.db.Exec(`INSERT INTO mus_quarantine (path, size_bytes, created_at) VALUES ('/q/a', ?, ?)`,
		250*spaceGiB, v.clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	s := v.newSpace(vols)
	spaceAllow(t, s, WorkSweep)
	v.clock.Advance(2 * time.Hour)
	s2 := v.newSpace(vols)
	spaceAllow(t, s2, WorkSweep)
	keys := alertKeys(v.notifier, AlertQuarantineFull)
	if len(keys) != 2 || keys[0] != keys[1] {
		t.Fatalf("quarantine-full keys = %v, want the same key twice (the notifier dedupes it)", keys)
	}
}
