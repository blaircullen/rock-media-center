package musicengine

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
)

func TestJournalBeginNamesTempDeterministicallyAndRefusesASecondOpenRow(t *testing.T) {
	v := newEnv(t)
	j := NewJournal(v.db, v.clock.Now)
	ctx := context.Background()
	dest := "/lib/Artist/Album/01. Song.flac"
	row, err := j.Start(ctx, JournalBegin{Kind: JournalPlace, NewPath: dest})
	if err != nil {
		t.Fatal(err)
	}
	wantTemp := filepath.Join("/lib/Artist/Album", ".rmc-tmp-"+itoa(row.ID)+"-01. Song.flac")
	if row.TempPath != wantTemp || row.Phase != PhaseNone || !row.Open() {
		t.Errorf("row = %+v, want temp %s at phase 0, open", row, wantTemp)
	}
	if _, err := j.Start(ctx, JournalBegin{Kind: JournalSwap, NewPath: dest}); !errors.Is(err, ErrJournalBusy) {
		t.Errorf("second Begin: %v, want ErrJournalBusy", err)
	}
	if err := j.Close(ctx, v.db, row.ID, "rolled back: test"); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Start(ctx, JournalBegin{Kind: JournalSwap, NewPath: dest}); err != nil {
		t.Errorf("Begin after close: %v", err)
	}
	if _, err := j.Start(ctx, JournalBegin{Kind: "bogus", NewPath: "/x"}); err == nil {
		t.Error("an unknown kind was accepted")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestJournalPhasesOnlyMoveForwardAndRecordOnlyVerifiedResults(t *testing.T) {
	v := newEnv(t)
	j := NewJournal(v.db, v.clock.Now)
	ctx := context.Background()
	row, _ := j.Start(ctx, JournalBegin{Kind: JournalPlace, NewPath: "/lib/A/B/01. T.flac"})

	if _, err := j.Record(ctx, v.db, row.ID, PhaseStaged, PhaseData{}); err == nil {
		t.Error("a phase with no checksum was recorded")
	}
	adv, err := j.Record(ctx, v.db, row.ID, PhaseTempVerified, PhaseData{TempSHA: "t"})
	if err != nil || !adv {
		t.Fatalf("record 2: %v %v", adv, err)
	}
	// An earlier phase after a later one is a no-op, not a step back.
	adv, err = j.Record(ctx, v.db, row.ID, PhaseStaged, PhaseData{StagedSHA: "s"})
	if err != nil || adv {
		t.Errorf("record 1 after 2: advanced %v err %v, want a harmless no-op", adv, err)
	}
	// Replaying the same phase is harmless too.
	if adv, err = j.Record(ctx, v.db, row.ID, PhaseTempVerified, PhaseData{TempSHA: "t"}); err != nil || adv {
		t.Errorf("replayed record 2: %v %v", adv, err)
	}
	cur, _ := j.Get(ctx, row.ID)
	if cur.Phase != PhaseTempVerified || cur.TempSHA != "t" || cur.StagedSHA != "" {
		t.Errorf("row = %+v", cur)
	}
	if err := j.Close(ctx, v.db, row.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(ctx, v.db, row.ID, "later"); err != nil {
		t.Fatal(err)
	}
	cur, _ = j.Get(ctx, row.ID)
	if cur.Open() || cur.LastError != "" {
		t.Errorf("closed twice changed the row: %+v", cur)
	}
	// A closed row takes no more phases.
	if adv, err = j.Record(ctx, v.db, row.ID, PhasePublished, PhaseData{PublishedSHA: "p"}); err != nil || adv {
		t.Errorf("record on a closed row: %v %v", adv, err)
	}
	if rows, _ := j.Unsettled(ctx); len(rows) != 0 {
		t.Errorf("unsettled = %v, want none", rows)
	}
}
