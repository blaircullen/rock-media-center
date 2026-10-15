import type { RequestTracking } from "./api";
import { formatAgo } from "./format";

// searchingDetail is the line under a request that's still being searched for, as its
// requester reads it: whether anything has turned up and when it was last checked. Times
// only — which trackers were asked, what they offered and why it was turned down is staff
// business, and a requester can't infer it from this.
//
//   never checked:            "Looking for a release"
//   checked, nothing yet:     "Looking for a release (checked 5m ago)"
//   searches came up empty:   "Still looking — nothing suitable yet (checked 2h ago)"
//   the server's own note:    "Not out yet"
export function searchingDetail(tr: RequestTracking, eps = "", now = Date.now()): string {
  const lastMs = tr.last_search_at ? Date.parse(tr.last_search_at) : NaN;
  const checked = Number.isFinite(lastMs) ? ` (checked ${formatAgo(lastMs, now)})` : "";
  if (tr.note) return tr.note + checked;
  if ((tr.misses ?? 0) > 0) return `Still looking — nothing suitable yet${checked}`;
  return (eps ? `${eps} · looking for more` : "Looking for a release") + checked;
}
