package applog

import (
	"net/http"
	"regexp"
	"strings"
)

// Request log lines say which kind of call was made, never which thing it was about.
// A path like /api/v1/media/movie/603 names the title someone opened; next to a "signed
// in" line, that's who looked at what. So request logging records the route pattern
// ("GET /api/v1/media/movie/{id}") and the names of the query parameters, not their values.

var idSegment = []*regexp.Regexp{
	regexp.MustCompile(`^(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`), // UUIDs (sessions, devices)
	regexp.MustCompile(`^u\d+$`),              // user ids
	regexp.MustCompile(`^(?i)[0-9a-f]{12,}$`), // long hex: hashes, tokens
	regexp.MustCompile(`\d`),                  // anything else carrying a number
}

// versionSegment is the one numbered segment worth keeping: /api/v1.
var versionSegment = regexp.MustCompile(`^v\d+$`)

// RedactPath replaces every path segment that looks like an identifier with {id}, for
// paths no route pattern matched. It errs towards hiding: losing a little detail in a
// 404 line is fine, naming a title is not.
func RedactPath(p string) string {
	if p == "" {
		return p
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if s == "" || versionSegment.MatchString(s) {
			continue
		}
		for _, re := range idSegment {
			if re.MatchString(s) {
				segs[i] = "{id}"
				break
			}
		}
	}
	return strings.Join(segs, "/")
}

// RouteLabel is what a request log line calls the request: the matched route pattern
// when there is one (Go's ServeMux fills r.Pattern in place, so it's visible after the
// handler has run), otherwise the method and the redacted path. A subtree pattern such
// as the catch-all "/" says nothing useful, so it falls back too.
func RouteLabel(r *http.Request) string {
	if p := r.Pattern; p != "" && !strings.HasSuffix(p, "/") {
		if !strings.Contains(p, " ") {
			p = r.Method + " " + p
		}
		return p
	}
	return r.Method + " " + RedactPath(r.URL.Path)
}
