import { test, expect, type MockedApi } from "./mockApi";
import type { MockRoute } from "./fixtures/routes";

// CFG-18: Settings → People & access is one list — role, auto-approve, Plex link, last
// seen, sessions, Disable and Delete — where each control saves itself.

test.describe("admin", () => {
  test.use({ persona: "admin" });

  test("one row per person with what they can do and when they were last around", async ({ page }) => {
    await page.goto("/settings/users");
    await expect(page.getByRole("heading", { name: "People & access" })).toBeVisible();
    const grandad = page.getByTestId("person").filter({ hasText: "Grandad" });
    await expect(grandad.getByText("Plex · grandadplex")).toBeVisible();
    await expect(grandad.getByText("Last seen 60 min ago")).toBeVisible();
    await expect(grandad.getByText("Signed in on 2 browsers")).toBeVisible();
    await expect(grandad.getByText("password set")).toBeVisible();
    // Your own row can't be disabled, deleted or signed out from here.
    const me = page.getByTestId("person").filter({ hasText: "(you)" });
    await expect(me.getByRole("button", { name: "Delete" })).toHaveCount(0);
    await expect(me.getByRole("button", { name: "Disable" })).toHaveCount(0);
  });

  test("role and Disable save from the row", async ({ page, api }) => {
    await page.goto("/settings/users");
    const grandad = page.getByTestId("person").filter({ hasText: "Grandad" });

    await grandad.getByRole("combobox", { name: "Grandad's role" }).selectOption("readonly");
    await expect.poll(() => api.callsTo("PUT", "/api/v1/users/5").map((c) => c.body)).toEqual([{ role: "readonly" }]);

    await grandad.getByRole("button", { name: "Disable" }).click();
    const dialog = page.getByRole("dialog", { name: "Turn off Grandad's sign-in?" });
    await dialog.getByRole("button", { name: "Turn off sign-in" }).click();
    await expect.poll(() => api.callsTo("PUT", "/api/v1/users/5").length).toBe(2);
    expect(api.callsTo("PUT", "/api/v1/users/5")[1].body).toEqual({ disabled: true });
  });

  test("Delete says what it removes, and deletes only once confirmed", async ({ page, api }) => {
    await installExtra(page, api, [
      { method: "GET", path: "/api/v1/users/5/impact", body: { requests: 4, sessions: 2, push_subscriptions: 0, plex_linked: true } },
      { method: "DELETE", path: "/api/v1/users/5", body: {} },
    ]);
    await page.goto("/settings/users");
    await page.getByTestId("person").filter({ hasText: "Grandad" }).getByRole("button", { name: "Delete" }).click();
    const dialog = page.getByRole("dialog", { name: "Delete Grandad?" });
    await expect(dialog.getByText(/signs them out of 2 browsers/)).toBeVisible();
    await expect(dialog.getByText(/Their 4 requests stay/)).toBeVisible();
    await dialog.getByRole("button", { name: "Delete user" }).click();
    await expect.poll(() => api.callsTo("DELETE", "/api/v1/users/5").length).toBe(1);
  });
});

// installExtra puts fixtures in front of the defaults for one test.
async function installExtra(page: import("@playwright/test").Page, api: MockedApi, extra: MockRoute[]) {
  await page.route((u) => u.pathname.startsWith("/api/"), async (route) => {
    const req = route.request();
    const url = new URL(req.url());
    for (const r of extra) {
      if (r.method !== req.method()) continue;
      const m = typeof r.path === "string" ? (url.pathname === r.path ? [] : null) : url.pathname.match(r.path);
      if (!m) continue;
      let body: unknown;
      try { body = req.postDataJSON(); } catch { body = req.postData(); }
      api.calls.push({ method: req.method(), path: url.pathname + url.search, body });
      const out = r.respond ? r.respond({ url, params: m.slice(1), body }) : r.body;
      return route.fulfill({ status: r.status ?? 200, contentType: "application/json", body: JSON.stringify(out ?? {}) });
    }
    return route.fallback();
  });
}
