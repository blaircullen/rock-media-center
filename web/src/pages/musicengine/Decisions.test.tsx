// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { musicEngineApi, type EngineDecision } from "../../lib/musicEngineApi";
import { clearQueryCache } from "../../lib/query";
import { ToastProvider } from "../../ui";
import { Decisions } from "./Decisions";

afterEach(() => { cleanup(); clearQueryCache(); vi.restoreAllMocks(); });

const dec = (p: Partial<EngineDecision> = {}): EngineDecision => ({
  id: 7, item_id: 3, kind: "no_lossless", reason: "No lossless copy found anywhere.",
  options: ["lossy", "wait", "drop"], created_at: 1_700_000_000, answer: null, answered_at: null,
  item: { title: "Roxanne", artist: "The Police", album: "Outlandos d'Amour", state: "waiting_blair" }, ...p,
});

const show = () => render(<ToastProvider><Decisions /></ToastProvider>);

describe("Decisions", () => {
  it("shows one plainly worded button per option", async () => {
    vi.spyOn(musicEngineApi, "decisions").mockResolvedValue([dec()]);
    show();
    expect(await screen.findByText("The Police – Roxanne")).toBeTruthy();
    expect(screen.getByText("No lossless copy found anywhere.")).toBeTruthy();
    for (const label of ["Take the YouTube audio (lossy)", "Keep waiting", "Drop it"]) expect(screen.getByRole("button", { name: label })).toBeTruthy();
  });

  it("clicking an option sends that option to the answer endpoint", async () => {
    vi.spyOn(musicEngineApi, "decisions").mockResolvedValue([dec()]);
    const answer = vi.spyOn(musicEngineApi, "answer").mockResolvedValue({ ignored: false, state: "searching" });
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Keep waiting" }));
    await waitFor(() => expect(answer).toHaveBeenCalledWith(7, "wait"));
  });

  it("labels use:<mbid>, treat_match and not_match options in plain words", async () => {
    vi.spyOn(musicEngineApi, "decisions").mockResolvedValue([
      dec({ options: ["use:aaaaaaaa-1", "treat_match", "not_match", "retry", "approve", "keep"] }),
    ]);
    show();
    await screen.findByText("The Police – Roxanne");
    for (const label of ["Use this recording", "It’s the same song", "It’s a different version", "Try again", "Swap it in", "Keep what I have"]) {
      expect(screen.getByRole("button", { name: label })).toBeTruthy();
    }
  });

  it("shows the server's reason when an answer is ignored", async () => {
    vi.spyOn(musicEngineApi, "decisions").mockResolvedValue([dec()]);
    vi.spyOn(musicEngineApi, "answer").mockResolvedValue({ ignored: true, reason: "That song was removed from the playlist.", state: "cancelled" });
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Drop it" }));
    expect(await screen.findByText("That song was removed from the playlist.")).toBeTruthy();
  });

  it("says so when the list can't load, instead of showing it empty", async () => {
    vi.spyOn(musicEngineApi, "decisions").mockRejectedValue(new Error("db is locked"));
    show();
    expect(await screen.findByText(/db is locked/)).toBeTruthy();
    expect(screen.queryByText("Nothing to decide")).toBeNull();
  });
});
