// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ApiError } from "../../lib/api";
import { musicEngineApi, type EngineSettings, type ReleaseChecks } from "../../lib/musicEngineApi";
import { clearQueryCache } from "../../lib/query";
import { ToastProvider } from "../../ui";
import { MusicEngineSettings } from "./MusicEngineSettings";

afterEach(() => { cleanup(); clearQueryCache(); vi.restoreAllMocks(); });

const base: EngineSettings = { watched_playlist: "spotify:playlist:abc", floor_gb: 50, margin_gb: 10, quarantine_share_gb: 200, auto_swaps: false, enabled: true, running: true, plex_section: "5", slskd_url: "http://gluetun:5030", gluetun_control_url: "http://gluetun:8000" };
const checks = (cal: boolean | null, path: boolean | null): ReleaseChecks => ({
  calibration: { passed: cal, at: cal === null ? null : 1_700_000_000, summary: cal ? "All 6 files matched." : "" },
  path_test: { passed: path, at: path === null ? null : 1_700_000_000, summary: path ? "Plex kept the same track." : "" },
});

const show = (c: ReleaseChecks, s: EngineSettings = base) => {
  vi.spyOn(musicEngineApi, "settings").mockResolvedValue(s);
  vi.spyOn(musicEngineApi, "releaseChecks").mockResolvedValue(c);
  render(<ToastProvider><MusicEngineSettings /></ToastProvider>);
};
const autoSwitch = () => screen.findByRole("switch", { name: "Automatic swaps" }) as Promise<HTMLButtonElement>;

describe("MusicEngineSettings", () => {
  it("keeps automatic swaps locked and shows each check's state until both have passed", async () => {
    show(checks(true, null));
    expect((await autoSwitch()).disabled).toBe(true);
    expect(screen.getByTestId("check-Calibration").textContent).toContain("Passed");
    expect(screen.getByTestId("check-Plex path test").textContent).toContain("Not run yet");
  });

  it("stays locked with only the path test passed", async () => {
    show(checks(null, true));
    expect((await autoSwitch()).disabled).toBe(true);
  });

  it("unlocks once both checks passed, and saves the switch", async () => {
    show(checks(true, true));
    const save = vi.spyOn(musicEngineApi, "saveSettings").mockResolvedValue({ ...base, auto_swaps: true });
    const sw = await autoSwitch();
    expect(sw.disabled).toBe(false);
    fireEvent.click(sw);
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(save).toHaveBeenCalledWith({ ...base, auto_swaps: true }));
  });

  it("shows the server's 409 message and puts the switch back when it refuses", async () => {
    show(checks(true, true));
    const msg = "automatic swaps stay off until both release checks pass";
    vi.spyOn(musicEngineApi, "saveSettings").mockRejectedValue(new ApiError(msg, 409, { error: msg }));
    fireEvent.click(await autoSwitch());
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText(msg)).toBeTruthy();
    expect((await autoSwitch()).getAttribute("aria-checked")).toBe("false");
  });

  it("says to restart when the saved switch and the running engine differ, and not otherwise", async () => {
    show(checks(null, null), { ...base, enabled: true, running: false });
    expect(await screen.findByText(/Restart Rock Media Center to apply/)).toBeTruthy();
    cleanup();
    clearQueryCache();
    show(checks(null, null), { ...base, enabled: true, running: true });
    await autoSwitch();
    expect(screen.queryByText(/Restart Rock Media Center to apply/)).toBeNull();
  });

  it("saves the Plex section and both addresses, and blocks a bad value before sending", async () => {
    show(checks(null, null));
    const save = vi.spyOn(musicEngineApi, "saveSettings").mockResolvedValue({ ...base, plex_section: "7" });
    await autoSwitch();
    fireEvent.change(screen.getByLabelText("Plex music section"), { target: { value: "seven" } });
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Plex music section"), { target: { value: "7" } });
    fireEvent.change(screen.getByLabelText("slskd address"), { target: { value: "gluetun:5030" } });
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("slskd address"), { target: { value: "https://slskd.lan:5030" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(save).toHaveBeenCalledWith({ ...base, plex_section: "7", slskd_url: "https://slskd.lan:5030" }));
  });

  it("never sends the read-only running flag (the server refuses unknown fields)", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify(base), { status: 200, headers: { "Content-Type": "application/json" } }));
    vi.stubGlobal("fetch", fetchMock);
    try {
      await musicEngineApi.saveSettings(base);
      const body = JSON.parse((fetchMock.mock.calls[0][1] as RequestInit).body as string);
      expect("running" in body).toBe(false);
      expect(body.plex_section).toBe("5");
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("records a path test result with what was seen", async () => {
    show(checks(null, null));
    const rec = vi.spyOn(musicEngineApi, "recordPathTest").mockResolvedValue(checks(null, true));
    await autoSwitch();
    fireEvent.change(screen.getByLabelText(/What you saw/), { target: { value: "Same track, new file." } });
    fireEvent.click(screen.getByRole("button", { name: "Record: it passed" }));
    await waitFor(() => expect(rec).toHaveBeenCalledWith(true, "Same track, new file."));
  });

  it("runs the calibration and lists each file's expected and actual verdict", async () => {
    show(checks(null, null));
    const run = vi.spyOn(musicEngineApi, "runCalibration").mockResolvedValue({
      passed: false, rows: [{ path: "/m/a.flac", expected: "genuine", actual: "upsampled", ok: false }],
    });
    await autoSwitch();
    fireEvent.change(screen.getByLabelText(/Files to check/), { target: { value: "/m/a.flac, genuine" } });
    fireEvent.click(screen.getByRole("button", { name: "Run calibration" }));
    await waitFor(() => expect(run).toHaveBeenCalledWith([{ path: "/m/a.flac", expected: "genuine" }]));
    expect(await screen.findByText(/expected genuine, got upsampled/)).toBeTruthy();
  });
});
