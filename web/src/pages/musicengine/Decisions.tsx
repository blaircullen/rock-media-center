import { useState } from "react";
import { Section } from "../../components/settings/ui";
import { musicEngineApi, optionLabel, type EngineDecision } from "../../lib/musicEngineApi";
import { usePoll } from "../../lib/usePoll";
import { useQuery } from "../../lib/query";
import { ago } from "../../lib/taskTime";
import { Button, EmptyState, ErrorState, useToast } from "../../ui";

// The decision list: things the engine won't settle on its own. One button per option the
// server offers; an answer is checked again against the song's current state, so the
// server may say it ignored the answer and why. That reason is shown as written.

export const DECISIONS_KEY = "musicengine:decisions";

const songName = (d: EngineDecision) => [d.item.artist, d.item.title].filter(Boolean).join(" – ") || `Item ${d.item_id}`;

export function Decisions({ onChanged }: { onChanged?: () => void }) {
  const { data, error, refetch } = useQuery(DECISIONS_KEY, musicEngineApi.decisions, { staleMs: 10_000 });
  usePoll(refetch, 20_000, { immediate: false });
  const toast = useToast();
  // The decision and option being sent, so only that button shows busy.
  const [sending, setSending] = useState<{ id: number; option: string } | null>(null);

  const answer = async (d: EngineDecision, option: string) => {
    setSending({ id: d.id, option });
    try {
      const r = await musicEngineApi.answer(d.id, option);
      if (r.ignored) toast(r.reason || "That answer was ignored.", { tone: "error" });
      else toast("Answer saved", { tone: "good" });
      await refetch();
      onChanged?.();
    } catch (e) {
      toast((e as Error).message, { tone: "error" });
      // A 409 means someone already answered: the list is stale, so reload it.
      await refetch();
    } finally {
      setSending(null);
    }
  };

  return (
    <Section id="decisions" title="Waiting for you" subtitle="Songs the engine needs you to decide on. Each one keeps its place until you answer.">
      {error && !data ? (
        <ErrorState what="the decision list" message={error.message} onRetry={() => void refetch()} />
      ) : !data ? (
        <p className="text-sm text-ink-dim">Loading…</p>
      ) : data.length === 0 ? (
        <EmptyState title="Nothing to decide" body="Songs that need your call will show up here." />
      ) : (
        <ul className="m-0 flex list-none flex-col gap-3 p-0">
          {data.map((d) => {
            const uses = d.options.filter((o) => o.startsWith("use:")).length;
            return (
              <li key={d.id} className="rounded-lg p-3" style={{ background: "var(--panel-2)", border: "1px solid var(--line)" }}>
                <div className="text-base font-semibold">{songName(d)}</div>
                {d.item.album && <div className="text-sm text-ink-dim">{d.item.album}</div>}
                <p className="my-1.5 text-sm">{d.reason}</p>
                <div className="mb-2 text-xs text-ink-faint">Asked {ago(d.created_at)}</div>
                <div className="flex flex-wrap gap-2">
                  {d.options.map((o) => (
                    <Button
                      key={o}
                      size="sm"
                      variant={o === "drop" ? "danger" : "secondary"}
                      disabled={sending?.id === d.id}
                      busy={sending?.id === d.id && sending.option === o}
                      onClick={() => void answer(d, o)}
                    >
                      {optionLabel(o)}
                      {o.startsWith("use:") && uses > 1 ? ` (${o.slice(4, 12)})` : ""}
                    </Button>
                  ))}
                </div>
              </li>
            );
          })}
        </ul>
      )}
    </Section>
  );
}
