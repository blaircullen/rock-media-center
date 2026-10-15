package requests

import (
	"context"
	"strconv"
)

// RequestersByLibrary names who is waiting on each library item: the owner and the
// followers of every open request (waiting for approval, or approved and not delivered
// yet), keyed "movie:12", "series:3" by the library id the request matches.
// Staff pages only (the Activity queue's "for Mum" chip): it pairs people with titles.
// A request the library doesn't hold yet has no key; a delivered one is no longer waited on.
func (s *Service) RequestersByLibrary(ctx context.Context) (map[string][]string, error) {
	open, err := s.repo.openRequesters(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	if len(open) == 0 {
		return out, nil
	}
	var movieIDs, seriesIDs []int
	for _, o := range open {
		switch o.mediaType {
		case "movie":
			movieIDs = append(movieIDs, o.tmdbID)
		case "series":
			seriesIDs = append(seriesIDs, o.tmdbID)
		}
	}
	movieLib := map[int]int64{}
	if s.movies != nil {
		for _, part := range chunks(movieIDs) {
			ms, err := s.movies.ByTMDBIDs(ctx, part)
			if err != nil {
				return nil, err
			}
			for _, m := range ms {
				movieLib[m.TMDBID] = m.ID
			}
		}
	}
	seriesLib := map[int]int64{}
	if s.series != nil {
		for _, part := range chunks(seriesIDs) {
			ss, err := s.series.ByTMDBIDs(ctx, part)
			if err != nil {
				return nil, err
			}
			for _, sr := range ss {
				seriesLib[sr.TMDBID] = sr.ID
			}
		}
	}
	seen := map[string]map[string]bool{}
	add := func(key, name string) {
		if name == "" {
			return
		}
		if seen[key] == nil {
			seen[key] = map[string]bool{}
		}
		if seen[key][name] {
			return
		}
		seen[key][name] = true
		out[key] = append(out[key], name)
	}
	for _, o := range open {
		var id int64
		switch o.mediaType {
		case "movie":
			id = movieLib[o.tmdbID]
		case "series":
			id = seriesLib[o.tmdbID]
		}
		if id <= 0 {
			continue
		}
		key := o.mediaType + ":" + strconv.FormatInt(id, 10)
		for _, n := range o.names {
			add(key, n)
		}
	}
	return out, nil
}

// openRequest is an open request's library keys and the people on it, owner first.
type openRequest struct {
	mediaType string
	tmdbID    int
	names     []string
}

// openRequesters reads every open request (waiting for approval, or in progress) with its
// owner's and followers' names, in one query.
func (r *Repo) openRequesters(ctx context.Context) ([]openRequest, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT q.id, q.media_type, q.tmdb_id,
		q.requested_by_name, COALESCE(s.user_name, '')
		FROM requests q LEFT JOIN request_subscribers s ON s.request_id = q.id
		WHERE q.`+knownKinds+` AND ((`+whereNeedsApproval+`) OR (`+whereInProgress+`))
		ORDER BY q.id, s.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []openRequest
	var lastID int64 = -1
	for rows.Next() {
		var id int64
		var o openRequest
		var owner, follower string
		if err := rows.Scan(&id, &o.mediaType, &o.tmdbID, &owner, &follower); err != nil {
			return nil, err
		}
		if id != lastID {
			o.names = []string{owner}
			out = append(out, o)
			lastID = id
		}
		if follower != "" {
			last := &out[len(out)-1]
			last.names = append(last.names, follower)
		}
	}
	return out, rows.Err()
}
