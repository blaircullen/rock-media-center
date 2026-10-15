// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ApiError } from "../../lib/api";
import { musicEngineApi, type EngineSwap } from "../../lib/musicEngineApi";
import { clearQueryCache } from "../../lib/query";
import { ToastProvider } from "../../ui";
import { Swaps } from "./Swaps";

afterEach(() => { cleanup(); clearQueryCache(); vi.restoreAllMocks(); });

const swap = (p: Partial<EngineSwap> = {}): EngineSwap => ({
  id: 4, item_id: 3, track_path: "/music/The Police/Roxanne.flac", created_at: 1_700_000_000, undone_at: null, undoable: true,
  old: { codec: "mp3", bit_depth: 0, sample_rate: 44100, bitrate: 320_000, verdict: "lossy" },
  new: { codec: "flac", bit_depth: 24, sample_rate: 96000, bitrate: 0, verdict: "genuine" }, ...p,
});

const show = () => render(<ToastProvider><Swaps /></ToastProvider>);

describe("Swaps", () => {
  it("shows what changed and undoes a swap", async () => {
    vi.spyOn(musicEngineApi, "swaps").mockResolvedValue([swap()]);
    const undo = vi.spyOn(musicEngineApi, "undoSwap").mockResolvedValue({ state: "done" });
    show();
    expect(await screen.findByText(/MP3 320 kbps → FLAC 24-bit 96 kHz/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Undo" }));
    await waitFor(() => expect(undo).toHaveBeenCalledWith(4));
  });

  it("shows the server's reason, verbatim, when undo is refused", async () => {
    vi.spyOn(musicEngineApi, "swaps").mockResolvedValue([swap()]);
    const reason = "a later swap changed this track; undo that one first";
    vi.spyOn(musicEngineApi, "undoSwap").mockRejectedValue(new ApiError(reason, 409, { error: reason, later_swap_id: 9 }));
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Undo" }));
    expect(await screen.findByText(reason)).toBeTruthy();
  });

  it("shows the cleared-from-quarantine refusal on a 410", async () => {
    vi.spyOn(musicEngineApi, "swaps").mockResolvedValue([swap()]);
    vi.spyOn(musicEngineApi, "undoSwap").mockRejectedValue(new ApiError("the old version was cleared from quarantine", 410));
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Undo" }));
    expect(await screen.findByText("the old version was cleared from quarantine")).toBeTruthy();
  });

  it("disables undo and shows why when the swap can't be undone", async () => {
    vi.spyOn(musicEngineApi, "swaps").mockResolvedValue([swap({ undoable: false, blocked_reason: "The old version was cleared from quarantine." })]);
    show();
    expect(await screen.findByText("The old version was cleared from quarantine.")).toBeTruthy();
    expect((screen.getByRole("button", { name: "Undo" }) as HTMLButtonElement).disabled).toBe(true);
  });
});
