import { describe, expect, it } from "vitest";
import type { RouteObject } from "react-router-dom";
import { MemoryRouter, Navigate } from "react-router-dom";
import { renderToStaticMarkup } from "react-dom/server";
import { buildRoutes, shellFor } from "./routes";
import { ModuleGate, moduleOn } from "../components/ModuleGate";
import type { RouteHandle } from "./title";

// paths flattens a route table to its page paths ("/" for the index route).
function routes(tree: RouteObject[]): RouteObject[] {
  return tree.flatMap((r) => [r, ...(r.children ? routes(r.children) : [])]);
}
function paths(tree: RouteObject[]): string[] {
  return routes(tree).filter((r) => r.path || r.index).map((r) => (r.index ? "/" : r.path!));
}

describe("shellFor", () => {
  it("gives staff the console wherever they sign in from", () => {
    expect(shellFor("admin", true)).toBe("staff");
    expect(shellFor("manager", false)).toBe("staff");
  });
  it("splits requesters by where they are", () => {
    expect(shellFor("requester", false)).toBe("requester");
    expect(shellFor("readonly", true)).toBe("external");
  });
});

describe("buildRoutes", () => {
  it("gives outside sessions no calendar", () => {
    const p = paths(buildRoutes({ role: "requester", external: true }));
    expect(p).toEqual(expect.arrayContaining(["/discover", "/requests", "/me", "*"]));
    expect(p).not.toContain("/calendar");
  });

  // APP-07: a title's own address exists in every shell, under /discover so the page
  // stays mounted beneath the sheet. REQ-19: the browse grid has the same overlays under it.
  it("gives every shell the Discover title addresses", () => {
    for (const [role, external] of [["admin", false], ["requester", false], ["requester", true]] as const) {
      const d = routes(buildRoutes({ role, external })).find((x) => x.path === "/discover");
      expect(d?.children?.map((c) => c.path), `${role} ${external}`).toEqual(["movie/:tmdbId", "series/:tmdbId", "person/:personId", "collection/:collectionId", "browse", "tv/:tmdbId"]);
      const browse = d?.children?.find((c) => c.path === "browse");
      expect(browse?.children?.map((c) => c.path), `${role} ${external} browse`).toEqual(["movie/:tmdbId", "series/:tmdbId", "person/:personId", "collection/:collectionId"]);
    }
  });

  it("keeps the console out of the requester shell", () => {
    const p = paths(buildRoutes({ role: "requester", external: false }));
    expect(p).toContain("/calendar");
    for (const staffOnly of ["/quality", "/settings/*", "/movies", "/logs", "/"]) expect(p).not.toContain(staffOnly);
  });

  it("serves the logs to admins only", () => {
    expect(paths(buildRoutes({ role: "admin", external: false }))).toContain("/logs");
    expect(paths(buildRoutes({ role: "manager", external: false }))).not.toContain("/logs");
  });

  it("offers Run setup again to admins only", () => {
    expect(paths(buildRoutes({ role: "admin", external: false }))).toContain("/setup");
    expect(paths(buildRoutes({ role: "manager", external: false }))).not.toContain("/setup");
    expect(paths(buildRoutes({ role: "requester", external: false }))).not.toContain("/setup");
  });

  it("keeps today's staff addresses and redirects", () => {
    const p = paths(buildRoutes({ role: "admin", external: false }));
    for (const want of ["/", "/activity/:tab", "/downloads", "/activity", "/history", "/review", "/blocklist", "/movies/:id", "/series/:id", "/music/album/:id",
      "/me", "/notifications", "/library", "/settings/*", "*"]) {
      expect(p).toContain(want);
    }
  });

  it("sends the old alert addresses to Settings → Alerts", () => {
    for (const path of ["/notifications", "/alerts"]) {
      const r = routes(buildRoutes({ role: "admin", external: false })).find((x) => x.path === path);
      expect((r?.element as { props?: { to?: string } })?.props?.to, path).toBe("/settings/alerts");
    }
  });

  it("titles every page and gives it an error card", () => {
    for (const role of ["admin", "requester"] as const) {
      const leaves = routes(buildRoutes({ role, external: false })).filter((r) => !r.children);
      for (const r of leaves) {
        expect(r.errorElement, r.path).toBeTruthy();
        // Redirects have no title of their own; everything else names the tab.
        const isRedirect = (r.element as { type?: unknown } | undefined)?.type === Navigate;
        if (!isRedirect) expect((r.handle as RouteHandle | undefined)?.title, r.path ?? "index").toBeTruthy();
      }
    }
  });
});

describe("ModuleGate", () => {
  it("reads the module's switch", () => {
    expect(moduleOn("music", { musicEnabled: true })).toBe(true);
    expect(moduleOn("music", { musicEnabled: false })).toBe(false);
  });

  // With no MeProvider, Music defaults off.
  it("redirects while the module is off", () => {
    const render = () =>
      renderToStaticMarkup(<MemoryRouter><ModuleGate module="music" home="/">page</ModuleGate></MemoryRouter>);
    expect(render()).toBe("");
  });

  it("has no routes for the removed books, audiobooks, subtitles and convert pages", () => {
    for (const [role, external] of [["admin", false], ["requester", false], ["requester", true]] as const) {
      const p = paths(buildRoutes({ role, external }));
      for (const gone of ["/books", "/shelf", "/audiobooks", "/subtitles", "/convert"]) expect(p, `${role} ${gone}`).not.toContain(gone);
    }
  });
});
