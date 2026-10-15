// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { musicEngineApi, type EngineItem } from "../lib/musicEngineApi";
import { clearQueryCache } from "../lib/query";
import { ConfirmProvider, ToastProvider } from "../ui";
import { MusicEngine } from "./MusicEngine";

const item = (id: number, state: string, p: Partial<EngineItem> = {}): EngineItem => ({
  id, kind: "request", state, last_reason: "", entered_at: 1_700_000_000, deadline_at: null, priority: 0,
  title: `Song ${id}`, artist: "Artist", album: "Album", recording_mbid: "", track_path: "", ...p,
});

beforeEach(() => {
  vi.spyOn(musicEngineApi, "decisions").mockResolvedValue([]);
  vi.spyOn(musicEngineApi, "swaps").mockResolvedValue([]);
  vi.spyOn(musicEngineApi, "lanes").mockResolvedValue([]);
  vi.spyOn(musicEngineApi, "space").mockResolvedValue({
    volumes: [], floor_bytes: 0, margin_bytes: 0, quarantine: { used_bytes: 0, share_bytes: 0, full: false }, intake_paused: false, sweep_paused: false,
  });
  vi.spyOn(musicEngineApi, "alertFailures").mockResolvedValue([]);
});
afterEach(() => { cleanup(); clearQueryCache(); vi.restoreAllMocks(); });

const show = () => render(<MemoryRouter><ToastProvider><ConfirmProvider><MusicEngine /></ConfirmProvider></ToastProvider></MemoryRouter>);

describe("MusicEngine page", () => {
  it("groups items by state with their reasons", async () => {
    vi.spyOn(musicEngineApi, "items").mockResolvedValue({
      items: [
        item(1, "searching", { last_reason: "Trying Usenet" }),
        item(2, "searching", { last_reason: "Trying Soulseek" }),
        item(3, "failed_check", { last_reason: "Upsampled from 44.1 kHz" }),
      ],
      next_before_id: null,
    });
    show();
    const searching = await screen.findByRole("region", { name: "Searching" });
    expect(searching.textContent).toContain("Trying Usenet");
    expect(searching.textContent).toContain("Trying Soulseek");
    expect(searching.textContent).not.toContain("Upsampled");
    const failed = screen.getByRole("region", { name: "Failed a check" });
    expect(failed.textContent).toContain("Upsampled from 44.1 kHz");
  });

  it("shows an error, not an empty list, when items can't load", async () => {
    vi.spyOn(musicEngineApi, "items").mockRejectedValue(new Error("boom"));
    show();
    expect(await screen.findByText(/boom/)).toBeTruthy();
    expect(screen.queryByText("No songs yet")).toBeNull();
  });

  it("shows which release 'get the full album' means before asking, then confirms", async () => {
    vi.spyOn(musicEngineApi, "items").mockResolvedValue({ items: [item(5, "done")], next_before_id: null });
    const album = vi.spyOn(musicEngineApi, "fullAlbum").mockImplementation(async (_id, confirm) => ({
      release: { artist: "The Police", title: "Outlandos d'Amour", edition: "Remastered", track_count: 10, year: 1978 },
      needs_confirm: !confirm, decision_id: null, state: "done",
    }));
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Get the full album" }));
    expect(await screen.findByText(/The Police – Outlandos d'Amour \(Remastered\), 10 tracks, 1978/)).toBeTruthy();
    expect(album).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button", { name: "Get the album" }));
    await waitFor(() => expect(album).toHaveBeenLastCalledWith(5, true));
  });
});
