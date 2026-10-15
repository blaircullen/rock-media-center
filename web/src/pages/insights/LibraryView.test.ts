import { describe, expect, it } from "vitest";
import { arrmadaPath } from "./LibraryView";

describe("Library links", () => {
  it("opens a title's Arrmada page only when Arrmada has it", () => {
    expect(arrmadaPath({ kind: "movie", title: "Dune", thumb_url: "", media: "movie", arrmada_id: 12 })).toBe("/movies/12");
    expect(arrmadaPath({ kind: "show", title: "Severance", thumb_url: "", media: "series", arrmada_id: 3 })).toBe("/series/3");
    expect(arrmadaPath({ kind: "movie", title: "Elsewhere", thumb_url: "" })).toBeNull();
  });
});
