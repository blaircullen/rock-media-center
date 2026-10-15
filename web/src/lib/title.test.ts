import { describe, expect, it } from "vitest";
import { formatTitle } from "./title";

describe("formatTitle", () => {
  it("prefers what the page knows over the route's name", () => {
    expect(formatTitle("Dune", "Movie")).toBe("Dune · Rock Media Center");
  });
  it("falls back to the route's title, then the app name", () => {
    expect(formatTitle("", "Movies")).toBe("Movies · Rock Media Center");
    expect(formatTitle("", "")).toBe("Rock Media Center");
  });
  it("leads with what's waiting on staff, capped", () => {
    expect(formatTitle("", "Movies", 3)).toBe("(3) Movies · Rock Media Center");
    expect(formatTitle("", "Movies", 0)).toBe("Movies · Rock Media Center");
    expect(formatTitle("", "", 120)).toBe("(99+) Rock Media Center");
  });
});
