package musicengine

import (
	"context"
	"errors"
	"fmt"

	"github.com/tristenlammi/arrmada/internal/audiocheck"
	"github.com/tristenlammi/arrmada/internal/metadata"
)

// TreatLibraryFileAsMatch is the answer "treat_match" to a library-check decision: Blair
// says the library file the tags could not settle is the recording. The decision stores
// only a sentence, not the file, so the file is found the same way the library check found
// it, the first unsettled candidate being the one the decision named. It is then
// audio-checked and the item finishes exactly as the library check finishes an item with a
// copy: genuine lossless ends it (done), a flawed or lossy copy makes it an upgrade
// (searching), and an unreadable file leaves everything as it was so the answer can be
// tried again.
func (e *Engine) TreatLibraryFileAsMatch(ctx context.Context, itemID int64) error {
	it, err := e.GetItem(ctx, itemID)
	if err != nil {
		return err
	}
	if it.State != StateWaitingBlair {
		return fmt.Errorf("item %d is %s, not waiting for an answer", it.ID, it.State)
	}
	if it.RecordingID == 0 {
		return fmt.Errorf("item %d has no pinned recording to match a library file against", it.ID)
	}
	if e.plex == nil || e.checker == nil || e.tags == nil || e.mb == nil {
		return errors.New("musicengine: treating a library file as the match needs Plex, MusicBrainz, a tag reader and the audio checker")
	}
	section := e.settings().PlexSection
	if section == "" {
		return errors.New("musicengine: no Plex music section configured")
	}
	rec, err := getRecording(ctx, e.db, it.RecordingID)
	if err != nil {
		return err
	}
	mctx := metadata.WithMBClass(ctx, mbClass(it))
	cands, err := e.libraryCandidates(ctx, section, it, rec)
	if err != nil {
		return err
	}

	var pick *libCand
	var pinnedFull *metadata.Recording
	for i := range cands {
		c := cands[i]
		res := matchYes
		if !c.known {
			r, _, err := e.classifyFile(mctx, rec, c, &pinnedFull)
			if err != nil {
				return err
			}
			res = r
		}
		if res == matchUnsettled {
			pick = &c
			break
		}
		if res == matchYes && pick == nil {
			pick = &c
		}
	}
	if pick == nil {
		return fmt.Errorf("the library file for item %d is no longer in the library; answer again after the next check", it.ID)
	}

	res := e.checker.Check(ctx, pick.path)
	cf := checkedFile{cand: *pick, res: res}
	var to State
	var newKind, why string
	switch {
	case res.Verdict == audiocheck.Blind:
		return fmt.Errorf("library file %s could not be read: %s", pick.path, res.Reason)
	case res.Verdict == audiocheck.Genuine && isLossless(res.Facts.Codec):
		to = StateDone
		why = fmt.Sprintf("Blair confirmed %s as the recording (%s)", pick.path, describeFacts(res))
	default:
		to = StateSearching
		why = fmt.Sprintf("Blair confirmed %s as the recording; it is %s (%s): upgrade", pick.path, res.Verdict, res.Reason)
		if res.Verdict == audiocheck.Genuine {
			why = fmt.Sprintf("Blair confirmed %s as the recording; it is lossy (%s): upgrade", pick.path, res.Facts.Codec)
		}
		if it.Kind == KindRequest {
			newKind = KindUpgrade
		}
	}

	// waiting_blair cannot go to done directly; library_check can, and is where the file
	// was being checked. Everything above only read, so a failure below leaves the item in
	// waiting_blair or library_check, both of which the engine handles.
	if err := e.Transition(ctx, it.ID, StateLibraryCheck, "Blair answered: treat the library file as the match"); err != nil {
		return err
	}
	cur, err := e.GetItem(ctx, it.ID)
	if err != nil {
		return err
	}
	return e.finishFromLibrary(ctx, cur, rec, cf, to, newKind, why)
}
