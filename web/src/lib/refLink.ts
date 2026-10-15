// Addresses for things in Discover, so a poster, a notification and a shared link all
// open the same place.

// titlePath is a movie's or show's own address: /discover/movie/603, /discover/series/1399.
// The segment is the API's media_type, so it maps 1:1 to /api/v1/media/<media>/<id>.
export function titlePath(media: "movie" | "series", tmdbId: number): string {
  return `/discover/${media}/${tmdbId}`;
}

// refToPath is where a notification's reference leads: the exact title, or the request
// itself. null for a reference it doesn't know (an old notice from before refs were
// structured, or about a kind of title Rock Media Center no longer has), which leads nowhere.
//
//   movie:603, movie:603:approved:1700000000      → /discover/movie/603
//   series:1399, series:1399:r12:s3                → /discover/series/1399
//   request:40:new:1700000000                      → /requests?id=40
//   issue:12:new                                   → /requests?tab=issues&issue=12
//
// Twin: RefPath in internal/requests/refpath.go (push links). Keep the two, and the test
// vectors in refLink.test.ts and refpath_test.go, in step.
export function refToPath(ref: string | undefined | null): string | null {
  if (!ref) return null;
  const at = ref.indexOf(":");
  if (at <= 0) return null;
  const kind = ref.slice(0, at);
  const rest = ref.slice(at + 1);
  switch (kind) {
    case "movie":
    case "series": {
      const id = rest.split(":")[0];
      return /^[1-9]\d*$/.test(id) ? titlePath(kind, Number(id)) : null;
    }
    case "request":
    case "issue": {
      const id = rest.split(":")[0];
      return /^[1-9]\d*$/.test(id) ? (kind === "issue" ? "/requests?tab=issues&issue=" : "/requests?id=") + id : null;
    }
  }
  return null;
}
