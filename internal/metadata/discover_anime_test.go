package metadata

import "testing"

// Anime never reaches Discover (Blair, 2026-10-15); other animation and Japanese live action
// still do.
func TestDiscoverDropsAnime(t *testing.T) {
	base := tmdbDiscoverItem{ID: 1, Name: "Show", PosterPath: "/p.jpg", VoteCount: 500, MediaType: "tv"}
	cases := []struct {
		name string
		lang string
		orig []string
		gen  []int
		keep bool
	}{
		{"japanese animation by language", "ja", nil, []int{16, 10759}, false},
		{"japanese animation by origin", "en", []string{"JP"}, []int{16}, false},
		{"western animation", "en", []string{"US"}, []int{16, 35}, true},
		{"japanese live action", "ja", []string{"JP"}, []int{18}, true},
	}
	for _, c := range cases {
		it := base
		it.OriginalLanguage, it.OriginCountry, it.GenreIDs = c.lang, c.orig, c.gen
		if _, ok := it.toItem("tv", 0, true); ok != c.keep {
			t.Errorf("%s: kept=%v, want %v", c.name, ok, c.keep)
		}
	}
}
