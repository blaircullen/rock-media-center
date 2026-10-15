import { describe, expect, it } from "vitest";
import { formatSeasons, requestStage, sortForRequester } from "./requestStage";
import type { MediaRequest } from "./api";

describe("formatSeasons", () => {
  it("collapses runs like the server's notices", () => {
    expect(formatSeasons([1, 2, 3, 5])).toBe("S1–3, S5");
    expect(formatSeasons([4])).toBe("S4");
    expect(formatSeasons([3, 1, 2, 2, 0])).toBe("S1–3");
    expect(formatSeasons([])).toBe("");
    expect(formatSeasons(undefined)).toBe("");
  });
});

describe("requestStage", () => {
  const at = "2026-10-01 09:00:00";
  const rq = (over: Partial<MediaRequest>): MediaRequest => ({
    id: 1, media_type: "movie", tmdb_id: 438631, title: "Dune", year: 2021, status: "approved",
    requested_by: 7, available: false, created_at: at, updated_at: at, ...over,
  });

  it("words a partly-ready request by the server's note, a series by its episodes", () => {
    expect(requestStage(rq({ tracking: { stage: "partial", note: "2 of 3 versions ready" } })).detail)
      .toBe("2 of 3 versions ready");
    expect(requestStage(rq({ media_type: "series", tracking: { stage: "partial", have: 3, total: 10 } })).detail)
      .toBe("3 of 10 episodes ready");
  });

  it("says a title waiting for Plex is almost ready, and sorts it with what's moving", () => {
    const s = requestStage(rq({ media_type: "movie", available: true, tracking: { stage: "adding" } }));
    expect([s.badge, s.detail]).toEqual(["Almost ready", "Adding to Plex…"]);
    const order = sortForRequester([
      rq({ id: 1, tracking: { stage: "available" } }),
      rq({ id: 2, tracking: { stage: "adding" } }),
      rq({ id: 3, tracking: { stage: "searching" } }),
    ]).map((r) => r.id);
    expect(order).toEqual([2, 3, 1]);
  });

  it("names the media server it's being added to, or your library with both", () => {
    const detail = (adding_to?: ("plex" | "jellyfin")[]) =>
      requestStage(rq({ media_type: "movie", available: true, tracking: { stage: "adding", adding_to } })).detail;
    expect(detail(["plex"])).toBe("Adding to Plex…");
    expect(detail(["jellyfin"])).toBe("Adding to Jellyfin…");
    expect(detail(["plex", "jellyfin"])).toBe("Adding to your library…");
    expect(detail()).toBe("Adding to Plex…");
  });
});
