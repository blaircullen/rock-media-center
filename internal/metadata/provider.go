// Package metadata resolves media metadata (titles, years, artwork, ids) from
// external providers. It's a first-class subsystem — the lesson from Readarr and
// Lidarr is never to depend on a single fragile source — so providers sit behind
// one interface and can be added/swapped without touching modules. TMDB is the
// first movie/TV provider.
package metadata

import (
	"context"
	"errors"
	"strings"
	"unicode"
)

// ErrNotConfigured means the provider is missing required config (e.g. API key).
var ErrNotConfigured = errors.New("metadata provider not configured")

// NormalizeTitle reduces a title to lowercase alphanumerics so titles compare
// free of punctuation, spacing, and case ("Marvel's Agents of S.H.I.E.L.D." →
// "marvelsagentsofshield"). Used to match a scanned folder to a search result.
func NormalizeTitle(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// TitleYearMatch picks the search result that confidently matches the given
// folder title (and year, when known): an exact normalized title wins, confirmed
// by year when the folder supplied one; a year-only match is accepted only as a
// fallback when the folder gave a year to anchor on. It returns ok=false rather
// than guess the most popular hit — guessing is what mis-files "UNTAMED" as the
// more popular "The Untamed". titleOf/yearOf project the caller's result type.
func TitleYearMatch[T any](results []T, title string, year int, titleOf func(T) string, yearOf func(T) int) (T, bool) {
	var zero T
	want := NormalizeTitle(title)
	if want == "" {
		return zero, false
	}
	if year > 0 {
		for _, r := range results {
			if NormalizeTitle(titleOf(r)) == want && absYear(yearOf(r)-year) <= 1 {
				return r, true
			}
		}
	}
	for _, r := range results {
		if NormalizeTitle(titleOf(r)) == want {
			return r, true
		}
	}
	// Fall back to a year match ONLY when the title is also a near miss. This used to
	// accept any result within a year of the folder's, with no title check whatsoever —
	// so a folder could be silently re-identified as a completely different show, taking
	// its existing files with it and then downloading the wrong series' episodes.
	if year > 0 {
		for _, r := range results {
			if absYear(yearOf(r)-year) <= 1 && titlesClose(NormalizeTitle(titleOf(r)), want) {
				return r, true
			}
		}
	}
	return zero, false
}

// maxTitleSuffix is how much extra text a "same show" title may carry — enough for a
// disambiguating year or country tag ("2022", "uk"), not enough for extra words. Any more
// and it's a different show: "lovedeath" and "lovedeathrobots" differ by six characters
// and are unrelated programmes.
const maxTitleSuffix = 4

// titlesClose reports whether two normalized titles are near-identical: one contained in
// the other, differing only by a short suffix. Containment alone is far too permissive —
// "lost" is contained in "lostinspace".
func titlesClose(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	long, short := a, b
	if len(short) > len(long) {
		long, short = short, long
	}
	if !strings.Contains(long, short) {
		return false
	}
	return len(long)-len(short) <= maxTitleSuffix
}

func absYear(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// MovieResult is a lightweight search hit.
type MovieResult struct {
	TMDBID      int     `json:"tmdb_id"`
	Title       string  `json:"title"`
	Year        int     `json:"year"`
	Overview    string  `json:"overview"`
	PosterURL   string  `json:"poster_url"`
	VoteAverage float64 `json:"vote_average"`
	// ReleaseDate is TMDB's release date (YYYY-MM-DD), when known.
	ReleaseDate string `json:"release_date,omitempty"`
}

// MovieDetails is a full movie record.
type MovieDetails struct {
	MovieResult
	IMDBID           string       `json:"imdb_id"`
	Runtime          int          `json:"runtime"`
	Status           string       `json:"status"`
	ReleaseDate      string       `json:"release_date,omitempty"`
	Genres           []string     `json:"genres,omitempty"`
	Studios          []string     `json:"studios,omitempty"`
	OriginalLanguage string       `json:"original_language,omitempty"`
	Certification    string       `json:"certification,omitempty"`
	BackdropURL      string       `json:"backdrop_url,omitempty"`
	CollectionID     int          `json:"collection_id,omitempty"`
	CollectionName   string       `json:"collection_name,omitempty"`
	Cast             []CastMember `json:"cast,omitempty"`
	// OriginalTitle is the film's original title when it's in the Latin alphabet ("" when
	// not); AltTitles the other titles its releases may be named by (movieAltTitles).
	OriginalTitle string   `json:"original_title,omitempty"`
	AltTitles     []string `json:"alt_titles,omitempty"`
}

// CastMember is one billed actor. ID is their TMDB person id (0 in records stored before
// ids were kept), which opens their page on Discover.
type CastMember struct {
	ID         int    `json:"id,omitempty"`
	Name       string `json:"name"`
	Character  string `json:"character,omitempty"`
	ProfileURL string `json:"profile_url,omitempty"`
}

// Collection is a movie franchise/collection and its member films (adult members never
// included).
type Collection struct {
	ID          int           `json:"id"`
	Name        string        `json:"name"`
	Overview    string        `json:"overview,omitempty"`
	PosterURL   string        `json:"poster_url,omitempty"`
	BackdropURL string        `json:"backdrop_url,omitempty"`
	Members     []MovieResult `json:"members"`
}

// MovieProvider looks up movie metadata.
type MovieProvider interface {
	// Available reports whether the provider is configured to answer queries.
	Available() bool
	SearchMovie(ctx context.Context, query string) ([]MovieResult, error)
	GetMovie(ctx context.Context, tmdbID int) (*MovieDetails, error)
	// GetCollection returns a collection's member movies (for "add whole collection").
	GetCollection(ctx context.Context, collectionID int) (*Collection, error)
}

// SeriesResult is a lightweight TV search hit.
type SeriesResult struct {
	TMDBID      int     `json:"tmdb_id"`
	Title       string  `json:"title"`
	Year        int     `json:"year"`
	Overview    string  `json:"overview"`
	PosterURL   string  `json:"poster_url"`
	VoteAverage float64 `json:"vote_average"`
}

// SeriesDetails is a full TV series record with its seasons and episodes.
type SeriesDetails struct {
	SeriesResult
	IMDBID      string          `json:"imdb_id,omitempty"`
	Status      string          `json:"status,omitempty"` // Returning Series | Ended | Canceled
	Network     string          `json:"network,omitempty"`
	BackdropURL string          `json:"backdrop_url,omitempty"`
	Genres      []string        `json:"genres,omitempty"`
	Cast        []CastMember    `json:"cast,omitempty"`
	Seasons     []SeasonDetails `json:"seasons,omitempty"`
	// OriginalName is TMDB's original_name: the title in the show's own language and script
	// (葬送のフリーレン for a Japanese show), so for anime it is usually NOT the romaji
	// title releases carry. The romaji form comes from AltTitles.
	OriginalName string `json:"original_name,omitempty"`
	// AltTitles are TMDB's alternative titles: romaji names, US/UK variant titles and
	// translations. The series module seeds release aliases from the useful ones.
	AltTitles    []AltTitle `json:"alt_titles,omitempty"`
	OriginalLang string     `json:"original_language,omitempty"`
	// OriginCountry is TMDB's origin_country (ISO 3166 codes: "US", "GB"), which tells
	// same-named shows apart when a release tags its country ("The.Office.US").
	OriginCountry []string `json:"origin_country,omitempty"`
	TVDBID        int      `json:"tvdb_id,omitempty"` // for TheXEM scene mapping

	// NumberingSource names whose listing Seasons came from: "tmdb", "tvdb" or "tvmaze".
	// NumberingFallback is set when that listing is NOT the one the show would normally
	// get — a higher-priority source that was available returned an error (or a TMDB
	// season failed to load) — so the series module must not renumber or move files on
	// it. Internal: never part of the API.
	NumberingSource   string `json:"-"`
	NumberingFallback bool   `json:"-"`
}

// AltTitle is one of TMDB's alternative titles for a show.
type AltTitle struct {
	Title   string `json:"title"`
	Country string `json:"country,omitempty"` // ISO 3166 code ("JP", "US")
	Type    string `json:"type,omitempty"`    // free text on TMDB: "Romaji", "romanization", ""…
}

// SeasonDetails is one season plus its episodes.
type SeasonDetails struct {
	SeasonNumber int              `json:"season_number"`
	Name         string           `json:"name,omitempty"`
	Overview     string           `json:"overview,omitempty"`
	PosterURL    string           `json:"poster_url,omitempty"`
	AirDate      string           `json:"air_date,omitempty"`
	Episodes     []EpisodeDetails `json:"episodes,omitempty"`
}

// EpisodeDetails is one episode.
type EpisodeDetails struct {
	EpisodeNumber int    `json:"episode_number"`
	Title         string `json:"title,omitempty"`
	Overview      string `json:"overview,omitempty"`
	AirDate       string `json:"air_date,omitempty"`
	Runtime       int    `json:"runtime,omitempty"`
	StillURL      string `json:"still_url,omitempty"`
	// AbsoluteNumber is the episode's position across the whole series, when the source
	// provides it authoritatively (TVDB does). 0 means "not supplied" — the series module
	// then derives it by counting, which is less reliable for anime.
	AbsoluteNumber int `json:"absolute_number,omitempty"`
}

// SeriesProvider looks up TV metadata.
type SeriesProvider interface {
	Available() bool
	SearchSeries(ctx context.Context, query string) ([]SeriesResult, error)
	GetSeries(ctx context.Context, tmdbID int) (*SeriesDetails, error)
}
