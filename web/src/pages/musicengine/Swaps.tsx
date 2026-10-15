import { useState } from "react";
import { Section } from "../../components/settings/ui";
import { describeSide, musicEngineApi, type EngineSwap } from "../../lib/musicEngineApi";
import { usePoll } from "../../lib/usePoll";
import { useQuery } from "../../lib/query";
import { ago } from "../../lib/taskTime";
import { Button, EmptyState, ErrorState, StatusChip, useToast } from "../../ui";

// Swaps: a better copy replacing a held one. Undo puts back the version that swap
// replaced. When the server can't, its reason is shown exactly as it sent it.

export const SWAPS_KEY = "musicengine:swaps";

const fileName = (p: string) => p.split("/").filter(Boolean).pop() ?? p;

export function Swaps({ onChanged }: { onChanged?: () => void }) {
  const { data, error, refetch } = useQuery(SWAPS_KEY, () => musicEngineApi.swaps(), { staleMs: 10_000 });
  usePoll(refetch, 30_000, { immediate: false });
  const toast = useToast();
  const [busyId, setBusyId] = useState<number | null>(null);
  // The server's refusal, kept on the row it belongs to until the next try.
  const [refused, setRefused] = useState<Record<number, string>>({});

  const undo = async (s: EngineSwap) => {
    setBusyId(s.id);
    setRefused((r) => Object.fromEntries(Object.entries(r).filter(([k]) => Number(k) !== s.id)));
    try {
      await musicEngineApi.undoSwap(s.id);
      toast("Swap undone", { tone: "good" });
      await refetch();
      onChanged?.();
    } catch (e) {
      setRefused((r) => ({ ...r, [s.id]: (e as Error).message }));
    } finally {
      setBusyId(null);
    }
  };

  return (
    <Section id="swaps" title="Swaps" subtitle="Songs where a better copy replaced the one you had. You can put the old one back.">
      {error && !data ? (
        <ErrorState what="the swaps" message={error.message} onRetry={() => void refetch()} />
      ) : !data ? (
        <p className="text-sm text-ink-dim">Loading…</p>
      ) : data.length === 0 ? (
        <EmptyState title="No swaps yet" />
      ) : (
        <ul className="m-0 flex list-none flex-col gap-2 p-0">
          {data.map((s) => (
            <li key={s.id} className="flex flex-col gap-1 rounded-lg p-3" style={{ background: "var(--panel-2)", border: "1px solid var(--line)" }}>
              <div className="flex flex-wrap items-center gap-2">
                <span className="min-w-0 flex-1 truncate text-base font-semibold" title={s.track_path}>{fileName(s.track_path)}</span>
                {s.undone_at ? (
                  <StatusChip tone="faint">Undone {ago(s.undone_at)}</StatusChip>
                ) : (
                  <Button size="sm" disabled={!s.undoable} busy={busyId === s.id} busyLabel="Undoing…" onClick={() => void undo(s)}>Undo</Button>
                )}
              </div>
              <div className="text-sm text-ink-dim">
                {describeSide(s.old)} → {describeSide(s.new)} · {ago(s.created_at)}
              </div>
              {!s.undone_at && !s.undoable && s.blocked_reason && <div className="text-sm text-ink-faint">{s.blocked_reason}</div>}
              {refused[s.id] && <div role="alert" className="text-sm" style={{ color: "var(--reject-text)" }}>{refused[s.id]}</div>}
            </li>
          ))}
        </ul>
      )}
    </Section>
  );
}
