import { useMemo, useState } from "react";
import { Section } from "../../components/settings/ui";
import { musicEngineApi, STATE_ORDER, stateLabel, type EngineItem } from "../../lib/musicEngineApi";
import { usePoll } from "../../lib/usePoll";
import { useQuery } from "../../lib/query";
import { ago, until } from "../../lib/taskTime";
import { Button, EmptyState, ErrorState, StatusChip, useConfirm, useToast, type Tone } from "../../ui";

// Every request and sweep item, grouped by state, each with its last reason and how long
// it has been in that state.

export const ITEMS_KEY = "musicengine:items";
const PAGE = 200;

const TONE: Record<string, Tone> = {
  waiting_blair: "avoid",
  failed_check: "reject",
  lane_unavailable: "reject",
  paused: "avoid",
  done: "good",
  dropped: "faint",
  cancelled: "faint",
};

const name = (i: EngineItem) => [i.artist, i.title].filter(Boolean).join(" – ") || i.track_path || `Item ${i.id}`;

export function ItemsTable() {
  const { data, error, refetch } = useQuery(ITEMS_KEY, () => musicEngineApi.items({ limit: PAGE }), { staleMs: 10_000 });
  usePoll(refetch, 15_000, { immediate: false });
  // Older pages the user asked for, kept across the page-one refresh.
  const [older, setOlder] = useState<EngineItem[]>([]);
  const [cursor, setCursor] = useState<number | null | undefined>(undefined);
  const [loadingMore, setLoadingMore] = useState(false);
  const [moreError, setMoreError] = useState<string | null>(null);
  const toast = useToast();
  const confirm = useConfirm();
  const [albumBusy, setAlbumBusy] = useState<number | null>(null);

  const next = cursor === undefined ? data?.next_before_id ?? null : cursor;

  const groups = useMemo(() => {
    const seen = new Set<number>();
    const all = [...(data?.items ?? []), ...older].filter((i) => !seen.has(i.id) && seen.add(i.id));
    const by = new Map<string, EngineItem[]>();
    for (const i of all) by.set(i.state, [...(by.get(i.state) ?? []), i]);
    const known = STATE_ORDER.map((o) => o.state);
    const order = [...known, ...[...by.keys()].filter((s) => !known.includes(s))];
    return order.filter((s) => by.has(s)).map((s) => ({ state: s, items: by.get(s)! }));
  }, [data, older]);

  const more = async () => {
    if (next == null) return;
    setLoadingMore(true);
    setMoreError(null);
    try {
      const p = await musicEngineApi.items({ limit: PAGE, before_id: next });
      setOlder((o) => [...o, ...p.items]);
      setCursor(p.next_before_id);
    } catch (e) {
      setMoreError((e as Error).message);
    } finally {
      setLoadingMore(false);
    }
  };

  // "Get the full album" shows which release it means before anything starts.
  const fullAlbum = async (i: EngineItem) => {
    setAlbumBusy(i.id);
    try {
      const first = await musicEngineApi.fullAlbum(i.id, false);
      const r = first.release;
      const label = `${r.artist} – ${r.title}${r.edition ? ` (${r.edition})` : ""}, ${r.track_count} tracks${r.year ? `, ${r.year}` : ""}`;
      if (first.needs_confirm) {
        const ok = await confirm({
          title: "Get the full album?",
          body: `This means ${label}. Every track goes through the same quality checks.`,
          confirmLabel: "Get the album",
        });
        if (!ok) return;
        await musicEngineApi.fullAlbum(i.id, true);
      }
      toast(`Getting ${label}`, { tone: "good" });
      await refetch();
    } catch (e) {
      toast((e as Error).message, { tone: "error" });
    } finally {
      setAlbumBusy(null);
    }
  };

  return (
    <Section id="items" title="Songs" subtitle="Every song the engine is working on, by what it is doing now.">
      {error && !data ? (
        <ErrorState what="the songs" message={error.message} onRetry={() => void refetch()} />
      ) : !data ? (
        <p className="text-sm text-ink-dim">Loading…</p>
      ) : groups.length === 0 ? (
        <EmptyState title="No songs yet" body="Songs show up here once the engine picks them up." />
      ) : (
        <>
          {groups.map((g) => (
            <section key={g.state} aria-label={stateLabel(g.state)} data-state={g.state}>
              <h3 className="m-0 mb-1.5 flex items-center gap-2 text-base font-semibold">
                {stateLabel(g.state)}
                <StatusChip tone={TONE[g.state] ?? "accent"} size="xs">{g.items.length}</StatusChip>
              </h3>
              <ul className="m-0 flex list-none flex-col p-0" style={{ borderTop: "1px solid var(--line)" }}>
                {g.items.map((i) => (
                  <li key={i.id} className="flex flex-wrap items-center gap-x-3 gap-y-1 py-2" style={{ borderBottom: "1px solid var(--line)" }}>
                    <div className="min-w-0 flex-1">
                      <div className="truncate text-base">{name(i)}</div>
                      {i.last_reason && <div className="text-sm text-ink-dim">{i.last_reason}</div>}
                    </div>
                    <div className="flex-none text-right text-xs text-ink-faint">
                      <div>{i.kind === "sweep" ? "Upgrade check" : "Request"} · {ago(i.entered_at)}</div>
                      {i.deadline_at ? <div>Next check {until(i.deadline_at)}</div> : null}
                    </div>
                    {i.state === "done" && (
                      <Button size="sm" variant="ghost" busy={albumBusy === i.id} busyLabel="Checking…" onClick={() => void fullAlbum(i)}>
                        Get the full album
                      </Button>
                    )}
                  </li>
                ))}
              </ul>
            </section>
          ))}
          {error && <p className="text-sm text-ink-faint">Refreshing failed: {error.message}</p>}
          {moreError && <p role="alert" className="text-sm" style={{ color: "var(--reject-text)" }}>{moreError}</p>}
          {next != null && (
            <div><Button onClick={() => void more()} busy={loadingMore} busyLabel="Loading…">Show older songs</Button></div>
          )}
        </>
      )}
    </Section>
  );
}
