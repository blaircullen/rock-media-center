package musicengine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tristenlammi/arrmada/internal/audiocheck"
)

// openDecisionKind returns the kind of the item's open decision ("" when none).
func (v *env) openDecisionKind(itemID int64) string {
	v.t.Helper()
	var kind string
	_ = v.db.QueryRow(`SELECT kind FROM mus_decisions WHERE item_id = ? AND answered_at = 0`, itemID).Scan(&kind)
	return kind
}

// More candidate files than the search reads: the genuine copy sits past the limit, so the
// search proves nothing and must not authorize a download (Astra 23).
func TestLibraryCheckPastTheCandidateLimitRaisesAmbiguityNotADownload(t *testing.T) {
	v := newEnv(t)
	it, _ := v.libItem()
	v.mb.add(mbRec("rec-other", "Other Song", "Artist", "", "ISRCOTHER"))
	for i := 0; i < maxLibraryCandidates; i++ {
		p := fmt.Sprintf("/mnt/nas/music/Artist/Other/%02d. Song.flac", i)
		v.plexHas(p, fmt.Sprintf("o%d", i))
		v.tags.tags[p] = Tags{RecordingMBID: "rec-other"}
	}
	past := "/mnt/nas/music/Artist/Real/99. Song.flac"
	v.plexHas(past, "real")
	v.tags.tags[past] = Tags{RecordingMBID: "rec-1"}
	v.checker.results[past] = []audiocheck.Result{genuineFLAC()}

	v.tick(1)

	got := v.item(it.ID)
	if got.State != StateWaitingBlair || v.openDecisionKind(it.ID) != "ambiguous" {
		t.Fatalf("item %s decision %q, want waiting_blair with an ambiguous decision (not searching)", got.State, v.openDecisionKind(it.ID))
	}
	if !strings.Contains(got.LastReason, "stopped at its limit") {
		t.Errorf("reason %q should say the search was cut short", got.LastReason)
	}
	if len(v.acquirer.calls) != 0 {
		t.Errorf("acquirer ran %d times, want no download", len(v.acquirer.calls))
	}
}

// Five lossy matches hit the match cap with more candidates unread: the lossy copy must not
// turn into an upgrade download while an adequate one may still be unread.
func TestLibraryCheckPastTheMatchLimitRaisesAmbiguityNotAnUpgrade(t *testing.T) {
	v := newEnv(t)
	it, _ := v.libItem()
	for i := 0; i <= maxMatchedChecks; i++ {
		p := fmt.Sprintf("/mnt/nas/music/Artist/Copy%d/01. Song.mp3", i)
		v.plexHas(p, fmt.Sprintf("c%d", i))
		v.tags.tags[p] = Tags{RecordingMBID: "rec-1"}
		v.checker.results[p] = []audiocheck.Result{verdictResult(audiocheck.Genuine, "mp3")}
	}

	v.tick(1)

	got := v.item(it.ID)
	if got.State != StateWaitingBlair || got.Kind != KindRequest || v.openDecisionKind(it.ID) != "ambiguous" {
		t.Fatalf("item %s kind %s decision %q, want waiting_blair / request / ambiguous", got.State, got.Kind, v.openDecisionKind(it.ID))
	}
}
