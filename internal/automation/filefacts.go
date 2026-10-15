package automation

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tristenlammi/arrmada/internal/movies"
	"github.com/tristenlammi/arrmada/internal/parser"
	"github.com/tristenlammi/arrmada/internal/quality"
	"github.com/tristenlammi/arrmada/internal/series"
)

// versionSizeBytes is the size the database records for a version's file: the cached
// media info's when present, else the version row's.
func versionSizeBytes(v movies.Version) int64 {
	if v.File != nil && v.File.SizeBytes > 0 {
		return v.File.SizeBytes
	}
	return v.SizeBytes
}

// currentMovieFile is a movie version's file as the upgrade and downgrade decisions judge
// it: its release (or the baseline built for a scanned file), its size and the movie's
// runtime, and what it was before an earlier version's conversion (read from older records).
func (c *Coordinator) currentMovieFile(ctx context.Context, m movies.Movie, v movies.Version) quality.CurrentFile {
	size := versionSizeBytes(v)
	return quality.CurrentFile{
		Release:     upgradeBaseline(m, v),
		SizeGB:      gbOf(size),
		RuntimeMin:  m.Runtime,
		OrigRelease: v.ConvertedFromRelease,
		OrigSizeGB:  gbOf(v.ConvertedFromSize),
	}
}

// CurrentMovieFile is the movie's default file as the upgrade sweep judges it — for the
// profile-change prompt, so "does this file still fit?" gets the same answer there.
func (c *Coordinator) CurrentMovieFile(ctx context.Context, m movies.Movie) quality.CurrentFile {
	v := movies.Version{
		IsDefault: true, HasFile: m.HasFile, FilePath: m.MovieFilePath,
		SourceRelease: m.SourceRelease, File: m.File,
		ConvertedFromRelease: m.ConvertedFromRelease, ConvertedFromSize: m.ConvertedFromSize,
	}
	return c.currentMovieFile(ctx, m, v)
}

// currentEpisodeFile is an episode's file as the upgrade decisions judge it: the release
// it was imported from — never the renamed library file (see upgradeSeries) — its size,
// the episode length, and what it was before an earlier version's conversion (read from older
// records).
func (c *Coordinator) currentEpisodeFile(ctx context.Context, e series.Episode) quality.CurrentFile {
	return c.episodeFileOf(ctx, series.EpisodeFile{
		Path: e.FilePath, SizeBytes: e.SizeBytes, SourceRelease: e.SourceRelease, RuntimeMin: e.Runtime,
		ConvertedFromRelease: e.ConvertedFromRelease, ConvertedFromSize: e.ConvertedFromSize,
	})
}

// episodeFileOf is currentEpisodeFile for an EpisodeFile row.
func (c *Coordinator) episodeFileOf(ctx context.Context, f series.EpisodeFile) quality.CurrentFile {
	return quality.CurrentFile{
		Release:     f.SourceRelease,
		SizeGB:      gbOf(f.SizeBytes),
		RuntimeMin:  f.RuntimeMin,
		OrigRelease: f.ConvertedFromRelease,
		OrigSizeGB:  gbOf(f.ConvertedFromSize),
	}
}

// upgradeBaseline is the "what we already have" release string the upgrade comparison scores
// against. Files Rock Media Center grabbed carry their SourceRelease; files found by a library scan don't —
// so fall back to their probed quality (e.g. "Bambi 1942 1080p BluRay x264"), then the filename.
// Without this, disk-imported movies could never be considered for an upgrade at all.
//
// The scanned-file baseline carries everything the cached media info knows — HDR, Atmos and
// lossless audio included. Without them a scanned file never met an
// HDR or Atmos target and was searched for an upgrade on every sweep.
func upgradeBaseline(m movies.Movie, v movies.Version) string {
	if s := strings.TrimSpace(v.SourceRelease); s != "" {
		return s
	}
	if v.File != nil && v.File.Quality != "" {
		parts := []string{m.Title}
		if m.Year > 0 {
			parts = append(parts, strconv.Itoa(m.Year))
		}
		parts = append(parts, v.File.Quality) // e.g. "1080p BluRay"
		if v.File.Codec != "" {
			parts = append(parts, v.File.Codec)
		}
		parts = append(parts, v.File.HDR...) // "HDR10", "DV", … as the parser names them
		if v.File.Atmos {
			parts = append(parts, "Atmos")
		}
		for _, a := range v.File.Audio {
			// Only the lossless ones: a lossy label adds nothing the target judges, and a
			// probed "eac3 5.1" would read as a format the file was never released as.
			for _, tag := range parser.Parse("x " + a).Audio {
				if tag == "TrueHD" || tag == "DTS-HD" || tag == "FLAC" {
					parts = append(parts, tag)
				}
			}
		}
		return strings.Join(parts, " ")
	}
	if v.FilePath != "" {
		return filepath.Base(v.FilePath)
	}
	return ""
}
