import { ApiError, req } from "./api";

// The lossless music engine's admin API (Music engine page and Settings → Music engine).
// Its own module, like lib/issuesApi.ts: only those lazy pages import it.
//
// The engine's routes describe a failure as {"error": "<plain sentence>"}; the rest of the
// app's server uses {"message": …}. call() lifts the engine's sentence into the thrown
// error's message so a page can show it as written.

export type ItemKind = "request" | "sweep";

export interface EngineItem {
  id: number;
  kind: ItemKind;
  state: string;
  last_reason: string;
  entered_at: number;
  deadline_at: number | null;
  priority: number;
  title: string;
  artist: string;
  album: string;
  recording_mbid: string;
  track_path: string;
}

export interface ItemsPage {
  items: EngineItem[];
  next_before_id: number | null;
}

export interface EngineDecision {
  id: number;
  item_id: number;
  kind: string;
  reason: string;
  options: string[];
  created_at: number;
  answer: string | null;
  answered_at: number | null;
  item: { title: string; artist: string; album: string; state: string };
}

export interface AnswerResult {
  ignored: boolean;
  reason?: string;
  state: string;
}

export interface SwapSide {
  codec: string;
  bit_depth: number;
  sample_rate: number;
  bitrate: number;
  verdict: string;
}

export interface EngineSwap {
  id: number;
  item_id: number;
  track_path: string;
  created_at: number;
  undone_at: number | null;
  undoable: boolean;
  blocked_reason?: string;
  old: SwapSide;
  new: SwapSide;
}

export type LaneName = "usenet" | "torrent" | "soulseek" | "youtube";
export interface EngineLane {
  lane: LaneName;
  status: "available" | "unavailable" | "searched";
  reason?: string;
  vpn_ok: boolean;
  checked_at: number | null;
  unavailable_since: number | null;
}

export interface EngineSpace {
  volumes: { name: "library" | "downloads"; free_bytes: number; measurable: boolean }[];
  floor_bytes: number;
  margin_bytes: number;
  quarantine: { used_bytes: number; share_bytes: number; full: boolean };
  intake_paused: boolean;
  sweep_paused: boolean;
  reason?: string;
}

export interface QuarantinePreview { count: number; bytes: number }
export interface QuarantineCleared { removed: number; freed_bytes: number }

export interface DeliveryFailure {
  event: string;
  connection: string;
  last_error: string;
  attempts: number;
  updated_at: number;
}

export interface FullAlbum {
  release: { artist: string; title: string; edition: string; track_count: number; year: number };
  needs_confirm: boolean;
  decision_id: number | null;
  state: string;
}

export interface EngineSettings {
  watched_playlist: string;
  floor_gb: number;
  margin_gb: number;
  quarantine_share_gb: number;
  auto_swaps: boolean;
  /** The saved switch. The engine is built only when the app starts, so it can differ from running. */
  enabled: boolean;
  /** Whether the engine was built at the last start. The server ignores it on save. */
  running: boolean;
  plex_section: string;
  slskd_url: string;
  gluetun_control_url: string;
}

export interface CheckResult { passed: boolean | null; at: number | null; summary: string }
export interface ReleaseChecks { calibration: CheckResult; path_test: CheckResult }

export interface CalibrationRow { path: string; expected: string }
export interface CalibrationResult {
  passed: boolean;
  rows: { path: string; expected: string; actual: string; ok: boolean }[];
}

const BASE = "/api/v1/musicengine";

async function call<T>(path: string, opts?: RequestInit): Promise<T> {
  try {
    return await req<T>(BASE + path, opts);
  } catch (e) {
    if (e instanceof ApiError && typeof e.body?.error === "string" && e.body.error) {
      throw new ApiError(e.body.error, e.status, e.body, e.path);
    }
    throw e;
  }
}

const post = (body: unknown): RequestInit => ({ method: "POST", body: JSON.stringify(body) });

export const musicEngineApi = {
  items: (p: { state?: string; limit?: number; before_id?: number } = {}) => {
    const q = new URLSearchParams();
    if (p.state) q.set("state", p.state);
    if (p.limit) q.set("limit", String(p.limit));
    if (p.before_id) q.set("before_id", String(p.before_id));
    const s = q.toString();
    return call<ItemsPage>(`/items${s ? `?${s}` : ""}`);
  },
  decisions: () => call<{ decisions: EngineDecision[] }>("/decisions?open=1").then((r) => r.decisions),
  answer: (id: number, option: string, note?: string) =>
    call<AnswerResult>(`/decisions/${id}/answer`, post(note ? { option, note } : { option })),
  swaps: (since?: number) => call<{ swaps: EngineSwap[] }>(`/swaps${since ? `?since=${since}` : ""}`).then((r) => r.swaps),
  undoSwap: (id: number) => call<{ state: string }>(`/swaps/${id}/undo`, { method: "POST" }),
  fullAlbum: (itemId: number, confirm: boolean) => call<FullAlbum>(`/items/${itemId}/full-album`, post({ confirm })),
  lanes: () => call<{ lanes: EngineLane[] }>("/lanes").then((r) => r.lanes),
  space: () => call<EngineSpace>("/space"),
  quarantinePreview: (olderThanDays: number) => call<QuarantinePreview>(`/quarantine/preview?older_than_days=${olderThanDays}`),
  quarantineClear: (olderThanDays: number) => call<QuarantineCleared>("/quarantine/clear", post({ older_than_days: olderThanDays })),
  alertFailures: () => call<{ failures: DeliveryFailure[] }>("/alerts/failures").then((r) => r.failures),
  settings: () => call<EngineSettings>("/settings"),
  // running is read-only; the server refuses a body that carries a field it does not take.
  saveSettings: (s: EngineSettings) => {
    const body: Partial<EngineSettings> = { ...s };
    delete body.running;
    return call<EngineSettings>("/settings", { method: "PUT", body: JSON.stringify(body) });
  },
  releaseChecks: () => call<ReleaseChecks>("/release-checks"),
  runCalibration: (rows: CalibrationRow[]) => call<CalibrationResult>("/release-checks/calibration", post({ rows })),
  recordPathTest: (passed: boolean, summary: string) => call<ReleaseChecks>("/release-checks/path-test", post({ passed, summary })),
};

// Plain words for what a decision's buttons do.
export function optionLabel(option: string): string {
  switch (option) {
    case "lossy": return "Take the YouTube audio (lossy)";
    case "wait": return "Keep waiting";
    case "drop": return "Drop it";
    case "approve": return "Swap it in";
    case "keep": return "Keep what I have";
    case "retry": return "Try again";
    case "treat_match": return "It’s the same song";
    case "not_match": return "It’s a different version";
    default:
      return option.startsWith("use:") ? "Use this recording" : option;
  }
}

// The words for an item state, in the order the page lists them.
export const STATE_ORDER: { state: string; label: string }[] = [
  { state: "waiting_blair", label: "Waiting for you" },
  { state: "failed_check", label: "Failed a check" },
  { state: "lane_unavailable", label: "Source unavailable" },
  { state: "paused", label: "Paused" },
  { state: "new", label: "Starting" },
  { state: "pinning", label: "Identifying the song" },
  { state: "library_check", label: "Checking the library" },
  { state: "searching", label: "Searching" },
  { state: "downloading", label: "Downloading" },
  { state: "checking", label: "Checking quality" },
  { state: "placing", label: "Adding to the library" },
  { state: "swapping", label: "Swapping" },
  { state: "waiting_for_playback", label: "Waiting for the song to stop playing" },
  { state: "awaiting_plex_index", label: "Waiting for Plex" },
  { state: "done", label: "Done" },
  { state: "dropped", label: "Dropped" },
  { state: "cancelled", label: "Cancelled" },
];

export const stateLabel = (s: string) => STATE_ORDER.find((o) => o.state === s)?.label ?? s.replace(/_/g, " ");

// "FLAC 16-bit 44.1 kHz", or "MP3 320 kbps" for a lossy file.
export function describeSide(s: SwapSide): string {
  const codec = (s.codec || "unknown").toUpperCase();
  if (s.bit_depth > 0) return `${codec} ${s.bit_depth}-bit ${+(s.sample_rate / 1000).toFixed(1)} kHz`;
  if (s.bitrate > 0) return `${codec} ${Math.round(s.bitrate / (s.bitrate > 10_000 ? 1000 : 1))} kbps`;
  return codec;
}
