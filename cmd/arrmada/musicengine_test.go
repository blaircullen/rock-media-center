package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/tristenlammi/arrmada/internal/musicengine"
)

// While the startup reconcile fails, a gated task body never runs and the error says why;
// once a retry succeeds the body runs, and the reconcile is not repeated (finding 16).
func TestReconcilerGateBlocksTasksUntilReconcileSucceeds(t *testing.T) {
	fail := true
	calls := 0
	rec := &reconciler{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		reconcile: func(context.Context) (musicengine.ReconcileReport, error) {
			calls++
			if fail {
				return musicengine.ReconcileReport{}, errors.New("library share unreachable")
			}
			return musicengine.ReconcileReport{}, nil
		},
	}
	ran := 0
	task := rec.gate(func(context.Context) error { ran++; return nil })

	err := task(context.Background())
	if err == nil || ran != 0 {
		t.Fatalf("gated task ran %d times with err %v while the reconcile failed, want 0 and an error", ran, err)
	}
	if rec.ensure(context.Background()) == nil {
		t.Fatal("ensure reported ready while the reconcile fails")
	}

	fail = false
	if err := task(context.Background()); err != nil || ran != 1 {
		t.Fatalf("after the reconcile succeeded: ran %d err %v, want 1 and nil", ran, err)
	}
	before := calls
	if err := task(context.Background()); err != nil || ran != 2 || calls != before {
		t.Fatalf("ready state: ran %d reconcile calls %d->%d, want it not to reconcile again", ran, before, calls)
	}
}
