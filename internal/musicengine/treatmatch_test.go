package musicengine

import (
	"context"
	"strings"
	"testing"

	"github.com/tristenlammi/arrmada/internal/audiocheck"
)

// unsettledItem leaves an item waiting_blair on a file whose tags cannot settle it.
func unsettledItem(t *testing.T) (*env, Item) {
	t.Helper()
	v := newEnv(t)
	it, _ := v.libItem()
	v.plexHas(songPath, "100")
	v.tags.tags[songPath] = Tags{Title: "Song", Artist: "Artist", Album: "Some Compilation", DurationSec: 200}
	return v, it
}

func TestTreatLibraryFileAsMatchFinishesTheItemFromTheFile(t *testing.T) {
	v, it := unsettledItem(t)
	v.checker.results[songPath] = []audiocheck.Result{genuineFLAC()}
	v.tick(1)
	if got := v.item(it.ID).State; got != StateWaitingBlair {
		t.Fatalf("state %s, want waiting_blair", got)
	}

	if err := v.e.TreatLibraryFileAsMatch(context.Background(), it.ID); err != nil {
		t.Fatal(err)
	}
	got := v.item(it.ID)
	if got.State != StateDone || got.TrackID == 0 || !strings.Contains(got.LastReason, songPath) {
		t.Fatalf("item = %s track %d reason %q, want done tied to the library file", got.State, got.TrackID, got.LastReason)
	}
	if len(v.acquirer.calls) != 0 {
		t.Fatal("treating the file as the match must not search")
	}
}

func TestTreatLibraryFileAsMatchMakesAFlawedFileAnUpgradeAndLeavesABlindReadAlone(t *testing.T) {
	v, it := unsettledItem(t)
	blind := audiocheck.Result{Verdict: audiocheck.Blind, Reason: "share offline"}
	v.checker.results[songPath] = []audiocheck.Result{blind, verdictResult(audiocheck.Suspect, "flac")}
	v.tick(1)

	if err := v.e.TreatLibraryFileAsMatch(context.Background(), it.ID); err == nil {
		t.Fatal("an unreadable file must fail the answer")
	}
	if got := v.item(it.ID).State; got != StateWaitingBlair {
		t.Fatalf("state %s after a blind read, want still waiting_blair", got)
	}
	if err := v.e.TreatLibraryFileAsMatch(context.Background(), it.ID); err != nil {
		t.Fatal(err)
	}
	got := v.item(it.ID)
	if got.State != StateSearching || got.Kind != KindUpgrade || got.TrackID == 0 {
		t.Fatalf("item = %s kind %s track %d, want searching / upgrade / tied", got.State, got.Kind, got.TrackID)
	}
}
