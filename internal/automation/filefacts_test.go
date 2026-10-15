package automation

import (
	"strings"
	"testing"

	"github.com/tristenlammi/arrmada/internal/movies"
	"github.com/tristenlammi/arrmada/internal/parser"
	"github.com/tristenlammi/arrmada/internal/quality"
)

// A library-scanned file has no recorded release, so its baseline is built from the
// cached media info — HDR, Atmos and lossless audio included, or it never meets an HDR
// or Atmos target and is searched for an upgrade every sweep.
func TestUpgradeBaselineCarriesProbedFormats(t *testing.T) {
	m := movies.Movie{Title: "Bambi", Year: 1942}
	v := movies.Version{IsDefault: true, HasFile: true, FilePath: "/m/Bambi (1942)/Bambi.mkv", File: &movies.MovieFile{
		Quality: "2160p BluRay", Codec: "hevc", HDR: []string{"DV", "HDR10"}, Atmos: true,
		Audio: []string{"truehd 7.1"},
	}}
	got := upgradeBaseline(m, v)
	for _, want := range []string{"Bambi 1942 2160p BluRay hevc", "DV", "HDR10", "Atmos", "TrueHD"} {
		if !strings.Contains(got, want) {
			t.Errorf("baseline %q is missing %q", got, want)
		}
	}
	f := quality.ReleaseFacts(parser.Parse(got), 0)
	if f.HDR != "HDR10" || !f.DolbyVision || !f.Atmos || !f.Lossless || f.Codec != "hevc" || f.Resolution != "2160p" {
		t.Errorf("the baseline doesn't read back as the file: %+v", f)
	}
	// A lossy track adds nothing.
	v.File.Audio, v.File.Atmos, v.File.HDR = []string{"eac3 5.1"}, false, nil
	if got := upgradeBaseline(m, v); got != "Bambi 1942 2160p BluRay hevc" {
		t.Errorf("baseline = %q", got)
	}
	// A recorded release always wins.
	v.SourceRelease = "Bambi.1942.2160p.BluRay.x265-GRP"
	if got := upgradeBaseline(m, v); got != v.SourceRelease {
		t.Errorf("baseline = %q, want the recorded release", got)
	}
}
