import { beforeEach, describe, expect, it, vi } from "vitest";

const status = vi.fn();
const restartApp = vi.fn();
vi.mock("./api", () => ({ api: { status: () => status(), restartApp: () => restartApp() } }));

import { restartAppAndWait, RESTART_TIMEOUT_MESSAGE } from "./restart";

describe("restartAppAndWait", () => {
  beforeEach(() => { status.mockReset(); restartApp.mockReset(); restartApp.mockResolvedValue({ status: "restarting" }); });

  it("waits past the old process (same started_at) and a down moment for the new one", async () => {
    status
      .mockResolvedValueOnce({ started_at: "old" }) // before the restart
      .mockResolvedValueOnce({ started_at: "old" }) // old process, still answering
      .mockRejectedValueOnce(new Error("down")) // between the two
      .mockResolvedValueOnce({ started_at: "new" });
    const then = vi.fn();
    await restartAppAndWait({ then, intervalMs: 1, timeoutMs: 5000 });
    expect(restartApp).toHaveBeenCalledOnce();
    expect(then).toHaveBeenCalledOnce();
    expect(status).toHaveBeenCalledTimes(4);
  });

  it("gives up with a pointer to the logs when the app never comes back", async () => {
    status.mockResolvedValue({ started_at: "old" });
    const then = vi.fn();
    await expect(restartAppAndWait({ then, intervalMs: 1, timeoutMs: 20 })).rejects.toThrow(RESTART_TIMEOUT_MESSAGE);
    expect(then).not.toHaveBeenCalled();
  });
});
