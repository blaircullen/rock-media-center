import { useState } from "react";
import { Section, Note } from "../../components/settings/ui";
import { Field } from "../../ui/Field";
import { Input } from "../../ui/Input";
import { formatBytes } from "../../lib/format";
import { musicEngineApi, type EngineLane } from "../../lib/musicEngineApi";
import { usePoll } from "../../lib/usePoll";
import { useQuery } from "../../lib/query";
import { ago } from "../../lib/taskTime";
import { Button, ErrorState, StatusChip, useConfirm, useToast, type Tone } from "../../ui";

// Where songs come from (lanes), how much room is left, the quarantine of replaced
// copies, and alerts that didn't get delivered.

const LANE_NAME: Record<string, string> = { usenet: "Usenet", torrent: "Torrents", soulseek: "Soulseek", youtube: "YouTube" };
const LANE_TONE: Record<EngineLane["status"], Tone> = { available: "good", searched: "accent", unavailable: "reject" };
const LANE_WORD: Record<EngineLane["status"], string> = { available: "Available", searched: "Searched", unavailable: "Unavailable" };

export const DEFAULT_CLEAR_DAYS = 90;

export function SpacePanel() {
  const lanes = useQuery("musicengine:lanes", musicEngineApi.lanes, { staleMs: 10_000 });
  const space = useQuery("musicengine:space", musicEngineApi.space, { staleMs: 10_000 });
  const failures = useQuery("musicengine:failures", musicEngineApi.alertFailures, { staleMs: 10_000 });
  usePoll(async () => { await Promise.all([lanes.refetch(), space.refetch(), failures.refetch()]); }, 30_000, { immediate: false });
  const toast = useToast();
  const confirm = useConfirm();
  const [days, setDays] = useState(String(DEFAULT_CLEAR_DAYS));
  const [clearing, setClearing] = useState(false);
  const n = Number(days);
  const daysOk = Number.isInteger(n) && n >= 0;

  // The count and size come from the server's preview of this exact age, before anything is removed.
  const clear = async () => {
    setClearing(true);
    try {
      const p = await musicEngineApi.quarantinePreview(n);
      if (p.count === 0) { toast("Nothing in quarantine is that old.", { tone: "info" }); return; }
      const ok = await confirm({
        title: "Clear old quarantined copies?",
        body: `This permanently removes ${p.count} ${p.count === 1 ? "copy" : "copies"} (${formatBytes(p.bytes)}) older than ${n} days. Swaps that used them can no longer be undone.`,
        confirmLabel: "Clear them",
        tone: "danger",
      });
      if (!ok) return;
      const r = await musicEngineApi.quarantineClear(n);
      toast(`Removed ${r.removed} ${r.removed === 1 ? "copy" : "copies"}, freed ${formatBytes(r.freed_bytes)}`, { tone: "good" });
      await space.refetch();
    } catch (e) {
      toast((e as Error).message, { tone: "error" });
    } finally {
      setClearing(false);
    }
  };

  const q = space.data?.quarantine;
  return (
    <>
      <Section id="lanes" title="Sources" subtitle="Where the engine looks for lossless copies.">
        {lanes.error && !lanes.data ? (
          <ErrorState what="the sources" message={lanes.error.message} onRetry={() => void lanes.refetch()} />
        ) : !lanes.data ? (
          <p className="text-sm text-ink-dim">Loading…</p>
        ) : (
          <ul className="m-0 flex list-none flex-col gap-2 p-0">
            {lanes.data.map((l) => (
              <li key={l.lane} className="flex flex-wrap items-center gap-2">
                <span className="w-24 text-base font-semibold">{LANE_NAME[l.lane] ?? l.lane}</span>
                <StatusChip tone={LANE_TONE[l.status]}>{LANE_WORD[l.status]}</StatusChip>
                {!l.vpn_ok && <StatusChip tone="reject">VPN down</StatusChip>}
                <span className="text-sm text-ink-dim">
                  {l.reason}
                  {l.status === "unavailable" && l.unavailable_since ? ` Since ${ago(l.unavailable_since)}.` : ""}
                </span>
              </li>
            ))}
          </ul>
        )}
      </Section>

      <Section id="space" title="Space" subtitle="The engine pauses new work when room runs low.">
        {space.error && !space.data ? (
          <ErrorState what="the space figures" message={space.error.message} onRetry={() => void space.refetch()} />
        ) : !space.data || !q ? (
          <p className="text-sm text-ink-dim">Loading…</p>
        ) : (
          <>
            {(space.data.intake_paused || space.data.sweep_paused) && (
              <Note tone="warn">
                {space.data.intake_paused ? "New requests are paused. " : ""}
                {space.data.sweep_paused ? "Upgrade checks are paused. " : ""}
                {space.data.reason}
              </Note>
            )}
            <ul className="m-0 flex list-none flex-col gap-1 p-0 text-base">
              {space.data.volumes.map((v) => (
                <li key={v.name}>
                  {v.name === "library" ? "Library" : "Downloads"}: {v.measurable ? `${formatBytes(v.free_bytes)} free` : "can’t be measured right now"}
                </li>
              ))}
              <li className="text-sm text-ink-dim">Stops at {formatBytes(space.data.floor_bytes)} free, with {formatBytes(space.data.margin_bytes)} of margin.</li>
            </ul>
            <div className="flex flex-col gap-2">
              <div className="text-base">
                Quarantine: {formatBytes(q.used_bytes)} of {formatBytes(q.share_bytes)}
                {q.full && <StatusChip tone="reject" className="ml-2">Full</StatusChip>}
              </div>
              <p className="m-0 text-sm text-ink-dim">Replaced copies wait here so a swap can be undone. Clearing them frees room and ends undo for those swaps.</p>
              <div className="flex flex-wrap items-end gap-3">
                <Field label="Older than (days)">
                  <Input type="number" min={0} className="w-28 px-3 py-2 text-base" value={days} onChange={(e) => setDays(e.target.value)} />
                </Field>
                <Button disabled={!daysOk} busy={clearing} busyLabel="Checking…" onClick={() => void clear()}>Clear old copies</Button>
              </div>
            </div>
          </>
        )}
      </Section>

      <Section id="alert-failures" title="Alerts that didn’t go out" subtitle="The engine keeps retrying these until they are delivered.">
        {failures.error && !failures.data ? (
          <ErrorState what="the alert failures" message={failures.error.message} onRetry={() => void failures.refetch()} />
        ) : !failures.data ? (
          <p className="text-sm text-ink-dim">Loading…</p>
        ) : failures.data.length === 0 ? (
          <p className="text-sm text-ink-dim">Every alert has been delivered.</p>
        ) : (
          <ul className="m-0 flex list-none flex-col gap-2 p-0">
            {failures.data.map((f) => (
              <li key={`${f.event}|${f.connection}`} className="text-sm">
                <span className="font-semibold">{f.event}</span> to {f.connection}: {f.last_error}
                <span className="text-ink-faint"> · {f.attempts} {f.attempts === 1 ? "try" : "tries"}, last {ago(f.updated_at)}</span>
              </li>
            ))}
          </ul>
        )}
      </Section>
    </>
  );
}
