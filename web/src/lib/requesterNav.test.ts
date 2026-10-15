import { describe, expect, it } from "vitest";
import { requesterNav, sectionFor } from "./requesterNav";

const header = (external: boolean) => requesterNav({ external }).filter((n) => n.header).map((n) => n.label);
const tabs = (external: boolean) => requesterNav({ external }).filter((n) => n.tab).map((n) => n.tabLabel);

describe("requesterNav", () => {
  it("is the top bar at home and from outside", () => {
    expect(header(false)).toEqual(["Discover", "Requests", "Calendar"]);
    expect(header(true)).toEqual(["Discover", "Requests"]);
  });

  // Requests has its own tab, so Calendar moves under Me.
  it("gives a phone Discover, Requests and Me, Me last, Calendar under Me", () => {
    for (const external of [false, true]) {
      expect(tabs(external)).toEqual(["Discover", "Requests", "Me"]);
    }
  });

  it("names the section an address belongs to", () => {
    const nav = requesterNav({ external: false });
    expect(sectionFor(nav, "/discover/movie/12")?.label).toBe("Discover");
    expect(sectionFor(nav, "/calendar")?.label).toBe("Calendar");
    expect(sectionFor(nav, "/me")?.label).toBe("Me");
    expect(sectionFor(nav, "/discovery")).toBeUndefined();
  });
});
