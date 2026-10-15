// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { type Indexer } from "../../lib/api";
import { indexerApi } from "../../lib/indexerApi";
import { IndexerEditor } from "./IndexerEditor";

const geek = (over: Partial<Indexer> = {}): Indexer => ({ id: 9, name: "NZBgeek", kind: "newznab", url: "https://api.nzbgeek.info/api", priority: 25, enabled: true, ...over });

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("the indexer editor's User-Agent field", () => {
  it("shows the saved User-Agent", () => {
    render(<IndexerEditor idx={geek({ user_agent: "Arrmada/1.0" })} usenet onSaved={() => {}} onChanged={() => {}} />);
    const form = screen.getByRole("form", { name: "Edit NZBgeek" });
    expect((within(form).getByLabelText("User-Agent (optional)") as HTMLInputElement).value).toBe("Arrmada/1.0");
  });

  it("saves a typed User-Agent, and a blank one when it is cleared", async () => {
    const upd = vi.spyOn(indexerApi, "updateIndexer").mockResolvedValue(undefined as never);
    render(<IndexerEditor idx={geek()} usenet onSaved={() => {}} onChanged={() => {}} />);
    const form = screen.getByRole("form", { name: "Edit NZBgeek" });
    fireEvent.change(within(form).getByLabelText("User-Agent (optional)"), { target: { value: "  Arrmada/1.0 " } });
    await act(async () => { fireEvent.click(within(form).getByRole("button", { name: "Save changes" })); });
    expect(upd).toHaveBeenLastCalledWith(9, expect.objectContaining({ user_agent: "Arrmada/1.0" }));
    fireEvent.change(within(form).getByLabelText("User-Agent (optional)"), { target: { value: "" } });
    await act(async () => { fireEvent.click(within(form).getByRole("button", { name: "Save changes" })); });
    expect(upd).toHaveBeenLastCalledWith(9, expect.objectContaining({ user_agent: "" }));
  });

  it("keeps the User-Agent when only a Used-for pill changes", async () => {
    const upd = vi.spyOn(indexerApi, "updateIndexer").mockResolvedValue(undefined as never);
    render(<IndexerEditor idx={geek({ user_agent: "Arrmada/1.0" })} usenet onSaved={() => {}} onChanged={() => {}} />);
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "TV" })); });
    expect(upd).toHaveBeenCalledWith(9, expect.objectContaining({ user_agent: "Arrmada/1.0" }));
  });
});
