package musicengine

import (
	"context"
	"strings"
	"testing"
)

// A song removed from Spotify after its download began keeps its item in waiting_blair (R4);
// its answer must still be ignored rather than acted on (Astra 20). A sweep item has no
// playlist entry by design and is still answered.
func TestAnswerIgnoresRemovedSongStillWaitingButNotASweepItem(t *testing.T) {
	v := newEnv(t)
	hooked := 0
	d := NewDecisions(v.e, AnswerHooks{TakeLossy: func(context.Context, Decision, Item) error { hooked++; return nil }})
	v.spotify.set(entry("sp1", "Song", "Artist", "I1"))
	v.sync()
	item := v.itemFor("sp1")
	id := v.decWaiting(item.ID, "no_genuine_copy", 0, decNoGenuine...)
	if _, err := v.db.Exec(`UPDATE mus_playlist_entries SET present = 0, removed_at = 1 WHERE item_id = ?`, item.ID); err != nil {
		t.Fatal(err)
	}
	res, err := d.Answer(context.Background(), id, OptionLossy, "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ignored || !strings.Contains(res.Reason, "removed from the Spotify playlist") || hooked != 0 {
		t.Fatalf("result %+v hooked=%d, want ignored and no hook call", res, hooked)
	}
	if st := v.item(item.ID).State; st != StateWaitingBlair {
		t.Errorf("ignored answer moved the item to %s", st)
	}
	if _, note, at := v.decAnswered(id); at == 0 || !strings.HasPrefix(note, "ignored: The song was removed") {
		t.Errorf("decision note %q answered_at %d, want closed with the reason", note, at)
	}

	v.spotify.set(entry("sp1", "Song", "Artist", "I1"), entry("sp2", "Other", "Artist", "I2"))
	v.sync()
	sw := v.itemFor("sp2")
	if _, err := v.db.Exec(`UPDATE mus_items SET kind = ? WHERE id = ?`, string(KindSweep), sw.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := v.db.Exec(`DELETE FROM mus_playlist_entries WHERE item_id = ?`, sw.ID); err != nil {
		t.Fatal(err)
	}
	id2 := v.decWaiting(sw.ID, "no_genuine_copy", 0, decNoGenuine...)
	res, err = d.Answer(context.Background(), id2, OptionLossy, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Ignored || hooked != 1 {
		t.Fatalf("sweep item answer %+v hooked=%d, want acted on", res, hooked)
	}
}
