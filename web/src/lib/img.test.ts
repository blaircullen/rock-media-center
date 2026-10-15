import { describe, expect, it } from "vitest";
import { posterThumb } from "./img";

describe("posterThumb", () => {
  it("passes empty values through", () => {
    expect(posterThumb(undefined)).toBeUndefined();
    expect(posterThumb("")).toBe("");
  });

  it("shrinks TMDB posters to w342", () => {
    expect(posterThumb("https://image.tmdb.org/t/p/w500/abc.jpg")).toBe("https://image.tmdb.org/t/p/w342/abc.jpg");
    expect(posterThumb("https://image.tmdb.org/t/p/original/abc.jpg")).toBe("https://image.tmdb.org/t/p/w342/abc.jpg");
  });

  it("leaves other URLs alone", () => {
    expect(posterThumb("/api/v1/music/albums/7/cover")).toBe("/api/v1/music/albums/7/cover");
    expect(posterThumb("https://example.com/cover-L.jpg")).toBe("https://example.com/cover-L.jpg");
  });
});
