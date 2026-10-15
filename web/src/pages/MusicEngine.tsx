import { Link } from "react-router-dom";
import { PageHeader } from "../components/PageHeader";
import { Page } from "../ui/Page";
import { invalidate } from "../lib/query";
import { Decisions } from "./musicengine/Decisions";
import { ItemsTable } from "./musicengine/ItemsTable";
import { SpacePanel } from "./musicengine/SpacePanel";
import { Swaps } from "./musicengine/Swaps";

// Music engine: what the lossless music engine is doing and what it needs from you. The
// decision list comes first because those songs are stuck until answered. The engine's own
// switches and release checks are on their own settings page.

// An answered decision or an undone swap changes the song list too.
const refreshLists = () => invalidate("musicengine:items");

export function MusicEngine() {
  return (
    <>
      <PageHeader title="Music engine" />
      <Page className="flex flex-col gap-6 py-6">
        <p className="m-0 text-sm text-ink-dim">
          Settings and the checks before automatic swaps are on the <Link to="/settings/music-engine" className="font-semibold" style={{ color: "var(--accent-text)" }}>Music engine settings</Link> page.
        </p>
        <Decisions onChanged={refreshLists} />
        <ItemsTable />
        <Swaps onChanged={refreshLists} />
        <SpacePanel />
      </Page>
    </>
  );
}
