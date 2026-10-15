import type { Artist, ImportRun, InsightsStats, Job, PlexConfig, TautulliConfig } from "../../src/lib/api";
import { NOW } from "./clock";
import type { PlexScanView } from "../../src/lib/plexLibraryApi";

// The optional modules (Insights, Music), idle and empty: the
// admin smoke only needs each page to load cleanly.

export const plexConfig: PlexConfig = { url: "", token_set: false, enabled: false, poll_seconds: 30, status: "unconfigured" };

// Settings → Media servers' Plex library updates card, with no Plex server connected.
export const plexScan: PlexScanView = {
  enabled: true, configured: false, path_map: [], last_scan: null, pending: 0,
  roots: [
    { kind: "movie", arrmada_root: "/movies", sections: [], plex_path: "" },
    { kind: "show", arrmada_root: "/tv", sections: [], plex_path: "" },
  ],
};

// Its Jellyfin library updates card, with no Jellyfin server connected either.
export const jellyfinScan: PlexScanView = { ...plexScan, roots: plexScan.roots.map((r) => ({ ...r })) };

// No imported watch history either, so Insights shows its setup state and nothing else.
export const insightsStats: InsightsStats = {
  most_watched_movies: [], most_watched_shows: [], most_active_users: [], most_active_platforms: [], recently_watched: [],
};

export const artists: { artists: Artist[] } = { artists: [] };

// Settings → Import: a saved Tautulli connection (key masked), one finished import and one
// that timed out, and the job an undo runs as.
const nowSecs = Math.floor(NOW / 1000);
export const tautulliConfig: TautulliConfig = { url: "http://192.168.50.247:8181", api_key_set: true };
export const importRuns: { runs: ImportRun[] } = {
  runs: [
    {
      id: 2, job_id: 41, source: "tautulli", started_at: nowSecs - 3600, finished_at: nowSecs - 1800, status: "timeout",
      total: 20000, processed: 15000, imported: 400, duplicates: 14590, overlaps: 10, invalid: 0, after_cutoff: 0, failed: 0,
      error: "the import took longer than 30 minutes and was stopped — Retry carries on from where it got to",
      cutoff_at: 0, rows: 400, removed_at: 0, removed_rows: 0,
    },
    {
      id: 1, job_id: 40, source: "tautulli", started_at: nowSecs - 86400, finished_at: nowSecs - 86000, status: "done",
      total: 13533, processed: 13533, imported: 12340, duplicates: 210, overlaps: 980, invalid: 3, after_cutoff: 0, failed: 0,
      error: "", cutoff_at: 0, rows: 12340, removed_at: 0, removed_rows: 0,
    },
  ],
};
export const removeImportJob: Job = {
  id: 950, kind: "insights.remove-import", target: "run:1", trigger: "user:1", status: "succeeded", progress: 1,
  message: "Removed 12340 imported plays", error: "",
  created_at: new Date(NOW).toISOString(), started_at: new Date(NOW).toISOString(), finished_at: new Date(NOW).toISOString(),
};
