package musicengine

import (
	"context"
	"testing"
)

// An intake request that merges into a sweep item promotes the survivor to an upgrade, so
// the sweep no longer closes its "no genuine copy" decision as a miss (Astra 24).
func TestMergeIntoSweepItemPromotesItToUpgradeAndSweepKeepsItsDecision(t *testing.T) {
	v := newEnv(t)
	v.mb.add(mbRec("rec-1", "Song", "Artist", "", "I1"))
	recID := v.seedRecording("rec-1", "Song", "Artist", "I1", "studio")
	sweepItem := v.seedItem(KindSweep, StateSearching, recID)

	v.spotify.set(entry("sp1", "Song", "Artist", "I1"))
	v.sync()
	v.tick(2)

	survivor := v.item(sweepItem)
	if survivor.Kind != KindUpgrade {
		t.Fatalf("survivor kind = %s, want %s (not request, not sweep)", survivor.Kind, KindUpgrade)
	}

	decID := v.decWaiting(sweepItem, DecisionNoGenuineCopy, 0, OptionRetry, OptionDrop)
	sw, err := NewSweep(SweepConfig{DB: v.db, Now: v.clock.Now, Local: func(p string) string { return p }})
	if err != nil {
		t.Fatal(err)
	}
	n, err := sw.settleMisses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("settleMisses closed %d decisions, want 0 for the merged intake item", n)
	}
	if _, _, at := v.decAnswered(decID); at != 0 {
		t.Error("the decision was closed as a sweep miss")
	}
	if st := v.item(sweepItem).State; st != StateWaitingBlair {
		t.Errorf("survivor state = %s, want waiting_blair", st)
	}
}
