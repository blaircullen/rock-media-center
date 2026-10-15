import { api } from "./api";

// Waiting out a restart. Docker's restart policy brings Rock Media Center back on its own; the page
// knows it's back when /api/v1/status reports a different started_at. (Polling for any
// answer isn't enough: the old process keeps answering for a moment while it shuts down.)

export interface WaitOptions {
  timeoutMs?: number; // give up after this long (default 2 minutes)
  intervalMs?: number; // between polls
  initialDelayMs?: number; // before the first poll, while the old process shuts down
  status?: () => Promise<{ started_at: string }>; // swappable for tests
  sleep?: (ms: number) => Promise<void>;
  now?: () => number;
}

const defaultSleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

// startedAt is the running process's start time, or "" when the server can't be reached.
export async function startedAt(status: () => Promise<{ started_at: string }> = api.status): Promise<string> {
  try { return (await status()).started_at; } catch { return ""; }
}

// waitForRestart resolves true once the server reports a start time other than before,
// or false when it hasn't come back within the timeout.
export async function waitForRestart(before: string, opts: WaitOptions = {}): Promise<boolean> {
  const { timeoutMs = 120_000, intervalMs = 1500, initialDelayMs = 2000, status = api.status, sleep = defaultSleep, now = Date.now } = opts;
  const start = now();
  await sleep(initialDelayMs);
  while (now() - start < timeoutMs) {
    const at = await startedAt(status);
    if (at && at !== before) return true;
    await sleep(intervalMs);
  }
  return false;
}

// restartAndWait notes when the server started, runs trigger (the call that makes it
// restart), then waits for the new process. The caller reloads the page afterwards.
export async function restartAndWait(trigger: () => Promise<unknown>, opts: WaitOptions = {}): Promise<boolean> {
  const before = await startedAt(opts.status);
  await trigger();
  return waitForRestart(before, opts);
}

// Fired after library folders are saved, so the restart banner re-checks at once instead
// of on the next page load.
export const FOLDERS_SAVED_EVENT = "arrmada:folders-saved";

export function announceFoldersSaved() {
  window.dispatchEvent(new Event(FOLDERS_SAVED_EVENT));
}

export const RESTART_TIMEOUT_MESSAGE = "Rock Media Center hasn't come back — check docker compose logs arrmada-app";

// restartAppAndWait restarts Rock Media Center (the app restarts itself in Docker) and, once the NEW
// process is answering, runs `then` (default: reload the page). Rejects with
// RESTART_TIMEOUT_MESSAGE when it hasn't come back within the timeout.
export async function restartAppAndWait(opts: { then?: () => void | Promise<void>; intervalMs?: number; timeoutMs?: number } = {}): Promise<void> {
  const { then = () => window.location.reload(), intervalMs = 1500, timeoutMs = 120_000 } = opts;
  const ok = await restartAndWait(() => api.restartApp(), { intervalMs, timeoutMs, initialDelayMs: intervalMs });
  if (!ok) throw new Error(RESTART_TIMEOUT_MESSAGE);
  await then();
}

// LIBRARY_LABEL names each folder the way Settings → Library does.
export const LIBRARY_LABEL: Record<string, string> = {
  movies: "Movies", tv: "TV", music: "Music", downloads: "Downloads",
};
