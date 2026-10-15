package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/tristenlammi/arrmada/internal/automation"
	"github.com/tristenlammi/arrmada/internal/indexer"
	"github.com/tristenlammi/arrmada/internal/quality"
)

// handleQualityTest runs a profile — as edited, saved or not — against a real title's
// indexer results: what it would grab, and why the rest were passed over.
//
//	{"profile": {...}, "movie_id": 12}
//	{"profile": {...}, "series_id": 4, "season": 2}
func (a *api) handleQualityTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Profile  quality.StoredProfile `json:"profile"`
		MovieID  int64                 `json:"movie_id"`
		SeriesID int64                 `json:"series_id"`
		Season   int                   `json:"season"`
	}
	if !a.decodeJSON(w, r, &req) {
		return
	}
	if a.deps.Automation == nil {
		a.writeError(w, http.StatusServiceUnavailable, "searching isn't available")
		return
	}
	ctx, cancel := context.WithTimeout(indexer.WithInteractive(r.Context()), 90*time.Second)
	defer cancel()
	var (
		list automation.ReleaseList
		err  error
	)
	switch {
	case req.MovieID > 0:
		list, err = a.deps.Automation.RankReleasesWith(ctx, req.MovieID, &req.Profile)
	case req.SeriesID > 0:
		season := req.Season
		if season <= 0 {
			season = 1
		}
		list, err = a.deps.Automation.RankSeriesReleasesWith(ctx, req.SeriesID, season, 0, &req.Profile)
	default:
		a.writeError(w, http.StatusBadRequest, "pick a movie or a show to test with")
		return
	}
	if err != nil {
		a.writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	a.writeJSON(w, http.StatusOK, list)
}
