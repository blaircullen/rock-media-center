package requests

import (
	"context"
	"fmt"
	"testing"

	"github.com/tristenlammi/arrmada/internal/jobs"
)

// recordJobs is a jobs.Submitter that keeps what it was given.
type recordJobs struct{ specs []jobs.Spec }

func (r *recordJobs) Submit(_ context.Context, s jobs.Spec) (int64, bool, error) {
	r.specs = append(r.specs, s)
	return int64(len(r.specs)), false, nil
}

// Approving a request starts its search as a job named for the title and the request, so
// a Search click on the same show while it runs joins it instead of searching twice.
func TestApproveSubmitsASearchJob(t *testing.T) {
	f := newApproveFixture(t, showListing(3))
	req, err := f.s.repo.Create(f.ctx, Request{MediaType: "series", TMDBID: 1399, Title: "Show", Status: StatusPending, RequestedBy: 7})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Approve(f.ctx, req.ID, ApproveOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(f.jobs.specs) != 1 {
		t.Fatalf("submitted %d jobs, want 1", len(f.jobs.specs))
	}
	sr, err := f.shows().GetByTMDB(f.ctx, 1399)
	if err != nil {
		t.Fatal(err)
	}
	sp := f.jobs.specs[0]
	if sp.Kind != "series.search" || sp.Target != fmt.Sprintf("series:%d", sr.ID) || sp.Trigger != fmt.Sprintf("request:%d", req.ID) ||
		sp.Class != jobs.ClassIndexerSearch || sp.Timeout == 0 {
		t.Fatalf("spec = %+v", sp)
	}
}
