package musicengine

import (
	"context"
	"errors"
	"fmt"
)

// The reconciler (KTD7). At boot, before any other engine work, it settles every open journal
// row from its last recorded phase and what is on disk, using the same Advance the steps use,
// so running it twice gives the same result (AE9):
//
//	phase none / staged     stray temp deleted, row closed as rolled back; the item stays in
//	                        placing/swapping and its next step starts a fresh row
//	phase temp_verified     redo the quarantine copy (swap/undo) and publish; a new track is
//	                        published by the no-replace link, or recognised as already published
//	phase quarantine_verified  re-check the destination still holds the incumbent, then publish
//	phase published         stray temp deleted; the item waits for Plex (awaiting_plex_index)
//	phase plex_confirmed    row closed
//
// A library that cannot be reached or measured makes Reconcile return an error without
// touching anything: rows stay as they are until the share is back.

// ReconcileReport counts what one run did.
type ReconcileReport struct {
	Rows        int
	RolledBack  int
	Stopped     int
	Advanced    int
	Closed      int
	ConfirmedBy int // rows without an item that Plex confirmed
}

// Reconcile settles every open journal row. Call it at boot, before the engine's first Tick.
func (p *Placement) Reconcile(ctx context.Context) (ReconcileReport, error) {
	var rep ReconcileReport
	if p.jr == nil {
		return rep, errors.New("musicengine placement: Reconcile before Bind")
	}
	rows, err := p.jr.Unsettled(ctx)
	if err != nil {
		return rep, err
	}
	if len(rows) == 0 {
		return rep, nil
	}
	if err := p.layout.Check(); err != nil {
		return rep, fmt.Errorf("reconcile: %d open journal rows left untouched: %w", len(rows), err)
	}
	var errs []error
	for _, row := range rows {
		rep.Rows++
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		if err := p.reconcileRow(ctx, row, &rep); err != nil {
			errs = append(errs, fmt.Errorf("journal row %d (%s %s, phase %d): %w", row.ID, row.Kind, row.NewPath, row.Phase, err))
		}
	}
	return rep, errors.Join(errs...)
}

func (p *Placement) reconcileRow(ctx context.Context, row JournalRow, rep *ReconcileReport) error {
	if row.Phase >= PhasePlexConfirmed {
		if err := p.jr.Close(ctx, p.m.DB(), row.ID, ""); err != nil {
			return err
		}
		rep.Closed++
		return nil
	}
	out, err := p.Advance(ctx, row)
	if err != nil {
		return err
	}
	switch out {
	case OutcomeRolledBack:
		rep.RolledBack++
		return nil
	case OutcomeStopped:
		rep.Stopped++
		return nil
	}
	rep.Advanced++
	// A published row with no item (an album track) has nobody to confirm it later: ask Plex
	// once now. Rows with an item are confirmed by that item's awaiting_plex_index step.
	cur, err := p.jr.Get(ctx, row.ID)
	if err != nil {
		return err
	}
	if cur.Phase != PhasePublished || cur.ItemID != 0 {
		return nil
	}
	tags, err := p.readTags(ctx, cur.NewPath)
	if err != nil {
		return fmt.Errorf("reading tags of %s: %w", cur.NewPath, err)
	}
	key, indexed, err := p.plexIndexed(ctx, cur, tags, false)
	if err != nil || !indexed {
		return err // not indexed yet: the row stays at published for the next run
	}
	if err := p.confirm(ctx, cur, tags, key, nil, false); err != nil {
		return err
	}
	rep.ConfirmedBy++
	return nil
}
