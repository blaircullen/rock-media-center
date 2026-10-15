// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { musicEngineApi } from "../../lib/musicEngineApi";
import { clearQueryCache } from "../../lib/query";
import { ConfirmProvider, ToastProvider } from "../../ui";
import { SpacePanel } from "./SpacePanel";

const GB = 1024 ** 3;

beforeEach(() => {
  vi.spyOn(musicEngineApi, "lanes").mockResolvedValue([
    { lane: "usenet", status: "available", vpn_ok: true, checked_at: 1_700_000_000, unavailable_since: null },
    { lane: "torrent", status: "unavailable", reason: "The VPN is down.", vpn_ok: false, checked_at: 1_700_000_000, unavailable_since: 1_700_000_000 },
  ]);
  vi.spyOn(musicEngineApi, "space").mockResolvedValue({
    volumes: [{ name: "library", free_bytes: 500 * GB, measurable: true }, { name: "downloads", free_bytes: 0, measurable: false }],
    floor_bytes: 50 * GB, margin_bytes: 10 * GB, quarantine: { used_bytes: 20 * GB, share_bytes: 200 * GB, full: false },
    intake_paused: false, sweep_paused: false,
  });
  vi.spyOn(musicEngineApi, "alertFailures").mockResolvedValue([
    { event: "Item past deadline", connection: "Telegram", last_error: "chat not found", attempts: 3, updated_at: 1_700_000_000 },
  ]);
});
afterEach(() => { cleanup(); clearQueryCache(); vi.restoreAllMocks(); });

const show = () => render(<ToastProvider><ConfirmProvider><SpacePanel /></ConfirmProvider></ToastProvider>);

describe("SpacePanel", () => {
  it("shows lanes with their reasons, space, and delivery failures", async () => {
    show();
    expect(await screen.findByText("Usenet")).toBeTruthy();
    expect(screen.getByText(/The VPN is down\./)).toBeTruthy();
    expect(screen.getByText("VPN down")).toBeTruthy();
    expect(await screen.findByText(/can’t be measured right now/)).toBeTruthy();
    expect(await screen.findByText(/chat not found/)).toBeTruthy();
  });

  it("asks before clearing quarantine and shows the count and size from the preview", async () => {
    const preview = vi.spyOn(musicEngineApi, "quarantinePreview").mockResolvedValue({ count: 12, bytes: 3 * GB });
    const clear = vi.spyOn(musicEngineApi, "quarantineClear").mockResolvedValue({ removed: 12, freed_bytes: 3 * GB });
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Clear old copies" }));
    expect(await screen.findByText(/removes 12 copies \(3\.0 GB\) older than 90 days/)).toBeTruthy();
    expect(preview).toHaveBeenCalledWith(90);
    expect(clear).not.toHaveBeenCalled(); // nothing removed until confirmed
    fireEvent.click(screen.getByRole("button", { name: "Clear them" }));
    await waitFor(() => expect(clear).toHaveBeenCalledWith(90));
  });

  it("removes nothing when the dialog is cancelled", async () => {
    vi.spyOn(musicEngineApi, "quarantinePreview").mockResolvedValue({ count: 2, bytes: GB });
    const clear = vi.spyOn(musicEngineApi, "quarantineClear").mockResolvedValue({ removed: 2, freed_bytes: GB });
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Clear old copies" }));
    fireEvent.click(await screen.findByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByText(/permanently removes/)).toBeNull());
    expect(clear).not.toHaveBeenCalled();
  });
});
