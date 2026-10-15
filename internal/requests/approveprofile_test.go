package requests

import (
	"strconv"
	"testing"

	"github.com/tristenlammi/arrmada/internal/metadata"
	"github.com/tristenlammi/arrmada/internal/movies"
	"github.com/tristenlammi/arrmada/internal/quality"
)

// A request whose stored profile has since been deleted is approved onto the default
// profile, while an approver who explicitly picks an unknown profile is still refused.
func TestApproveDanglingStoredProfileUsesDefault(t *testing.T) {
	f := newApproveFixture(t, metadata.SeriesDetails{})
	s, ctx := f.s, f.ctx
	sp, err := s.quality.Create(ctx, quality.StoredProfile{MediaType: quality.MediaMovie, Name: "Films"})
	if err != nil {
		t.Fatal(err)
	}
	def := "custom:" + strconv.FormatInt(sp.ID, 10)
	if err := s.quality.SetDefaultProfile(ctx, quality.MediaMovie, def); err != nil {
		t.Fatal(err)
	}
	// Already in the library, so the approve takes the ErrExists path.
	if _, err := movies.NewRepo(s.repo.db).Create(ctx, movies.Movie{TMDBID: 949, Title: "Heat", Year: 1995, QualityProfile: def}); err != nil {
		t.Fatal(err)
	}
	req, err := s.repo.Create(ctx, Request{MediaType: "movie", TMDBID: 949, Title: "Heat",
		Status: StatusPending, QualityProfile: "custom:999", RequestedBy: 7, RequestedByName: "alice"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Approve(ctx, req.ID, ApproveOptions{Profile: "custom:998"}); err != ErrUnknownProfile {
		t.Fatalf("explicit unknown profile: err = %v, want ErrUnknownProfile", err)
	}
	got, err := s.Approve(ctx, req.ID, ApproveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusApproved || got.QualityProfile != def {
		t.Errorf("approved = status %q profile %q, want approved on the default %q", got.Status, got.QualityProfile, def)
	}
}
