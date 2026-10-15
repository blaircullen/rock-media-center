import { useEffect, useState } from "react";
import { Note, Section } from "../../components/settings/ui";
import { Field } from "../../ui/Field";
import { Input } from "../../ui/Input";
import { Switch } from "../../ui/Switch";
import { Textarea } from "../../ui/Textarea";
import { musicEngineApi, type CalibrationResult, type CheckResult, type EngineSettings } from "../../lib/musicEngineApi";
import { invalidate, useQuery } from "../../lib/query";
import { ago } from "../../lib/taskTime";
import { Button, ErrorState, StatusChip, useToast } from "../../ui";

// Settings → Music engine: the engine's own keys, and the two checks that must pass before
// automatic swaps can be turned on. The server enforces that rule too, so a refusal from it
// is shown as it was sent.

const SETTINGS_KEY = "musicengine:settings";
const CHECKS_KEY = "musicengine:checks";

const isHttpUrl = (v: string) => {
  try {
    const u = new URL(v.trim());
    return (u.protocol === "http:" || u.protocol === "https:") && u.host !== "";
  } catch {
    return false;
  }
};

const numberField = (v: string) => (v.trim() === "" ? NaN : Number(v));

export function MusicEngineSettings() {
  const settings = useQuery(SETTINGS_KEY, musicEngineApi.settings, { staleMs: 0 });
  const checks = useQuery(CHECKS_KEY, musicEngineApi.releaseChecks, { staleMs: 0 });
  const toast = useToast();
  const [form, setForm] = useState<EngineSettings | null>(null);
  // What the server has saved, for the restart notice (the form may hold unsaved edits).
  const [saved, setSaved] = useState<EngineSettings | null>(null);
  const [saving, setSaving] = useState(false);
  const [refusal, setRefusal] = useState<string | null>(null);

  useEffect(() => {
    if (settings.data) {
      setForm((f) => f ?? settings.data!);
      setSaved(settings.data);
    }
  }, [settings.data]);

  const bothPassed = checks.data?.calibration.passed === true && checks.data?.path_test.passed === true;

  const save = async () => {
    if (!form) return;
    setSaving(true);
    setRefusal(null);
    try {
      const result = await musicEngineApi.saveSettings(form);
      setForm(result);
      setSaved(result);
      invalidate(SETTINGS_KEY);
      toast("Saved", { tone: "good" });
    } catch (e) {
      setRefusal((e as Error).message);
      // Put the switch back to what is actually saved.
      setForm((f) => (f && settings.data ? { ...f, auto_swaps: settings.data.auto_swaps } : f));
    } finally {
      setSaving(false);
    }
  };

  const set = <K extends keyof EngineSettings>(k: K, v: EngineSettings[K]) => setForm((f) => (f ? { ...f, [k]: v } : f));
  const gb = (k: "floor_gb" | "margin_gb" | "quarantine_share_gb", label: string, hint: string) => (
    <Field label={label} hint={hint}>
      <Input type="number" min={0} className="w-32 px-3 py-2 text-base" value={Number.isNaN(form![k]) ? "" : form![k]} onChange={(e) => set(k, numberField(e.target.value))} />
    </Field>
  );
  const sectionOk = !!form && /^[1-9][0-9]*$/.test(form.plex_section.trim());
  const slskdOk = !!form && isHttpUrl(form.slskd_url);
  const gluetunOk = !!form && isHttpUrl(form.gluetun_control_url);
  const valid = !!form && [form.floor_gb, form.margin_gb, form.quarantine_share_gb].every((n) => Number.isFinite(n) && n >= 0) && sectionOk && slskdOk && gluetunOk;
  const needsRestart = !!saved && saved.enabled !== saved.running;

  return (
    <div className="flex flex-col gap-6">
      <Section id="music-engine" title="Music engine" subtitle="Gets lossless copies of the songs in your watched playlist and swaps in better ones.">
        {settings.error && !settings.data ? (
          <ErrorState what="the engine settings" message={settings.error.message} onRetry={() => void settings.refetch()} />
        ) : !form ? (
          <p className="text-sm text-ink-dim">Loading…</p>
        ) : (
          <>
            <Switch checked={form.enabled} onChange={(v) => set("enabled", v)}>Engine on</Switch>
            {needsRestart && (
              <Note tone="info">
                The engine is {saved!.running ? "running" : "off"} but saved as {saved!.enabled ? "on" : "off"}. Restart Rock Media Center to apply.
              </Note>
            )}
            <Field label="Watched playlist" hint="The Spotify playlist the engine follows.">
              <Input value={form.watched_playlist} onChange={(e) => set("watched_playlist", e.target.value)} />
            </Field>
            <Field label="Plex music section" hint="The number of your music library in Plex." error={sectionOk ? undefined : "Enter a whole number, 1 or more."}>
              <Input inputMode="numeric" className="w-32 px-3 py-2 text-base" value={form.plex_section} onChange={(e) => set("plex_section", e.target.value)} />
            </Field>
            <Field label="slskd address" hint="Where the Soulseek client answers, for example http://gluetun:5030." error={slskdOk ? undefined : "Enter a full http:// or https:// address."}>
              <Input value={form.slskd_url} onChange={(e) => set("slskd_url", e.target.value)} />
            </Field>
            <Field label="VPN control address" hint="The gluetun control server the engine asks whether the VPN is up." error={gluetunOk ? undefined : "Enter a full http:// or https:// address."}>
              <Input value={form.gluetun_control_url} onChange={(e) => set("gluetun_control_url", e.target.value)} />
            </Field>
            {gb("floor_gb", "Free space floor (GB)", "New work pauses below this much free space.")}
            {gb("margin_gb", "Safety margin (GB)", "Extra room kept on top of the floor.")}
            {gb("quarantine_share_gb", "Quarantine share (GB)", "How much room replaced copies may use. Upgrade checks pause when it is full.")}
            <div className="flex flex-col gap-1.5">
              <Switch checked={form.auto_swaps} disabled={!bothPassed && !form.auto_swaps} onChange={(v) => set("auto_swaps", v)}>Automatic swaps</Switch>
              <span className="text-xs text-ink-faint">
                {bothPassed ? "Both release checks passed." : "Stays off until both release checks below have passed."} Anything the engine is unsure about still waits for you.
              </span>
            </div>
            {refusal && <div role="alert" className="text-sm" style={{ color: "var(--reject-text)" }}>{refusal}</div>}
            <div><Button variant="primary" onClick={() => void save()} busy={saving} busyLabel="Saving…" disabled={!valid}>Save</Button></div>
          </>
        )}
      </Section>

      <Section id="release-checks" title="Release checks" subtitle="Two checks to run once before automatic swaps are allowed.">
        {checks.error && !checks.data ? (
          <ErrorState what="the release checks" message={checks.error.message} onRetry={() => void checks.refetch()} />
        ) : !checks.data ? (
          <p className="text-sm text-ink-dim">Loading…</p>
        ) : (
          <>
            <Calibration result={checks.data.calibration} onDone={() => void checks.refetch()} />
            <PathTest result={checks.data.path_test} onDone={() => void checks.refetch()} />
          </>
        )}
      </Section>
    </div>
  );
}

function CheckState({ name, result }: { name: string; result: CheckResult }) {
  const tone = result.passed === true ? "good" : result.passed === false ? "reject" : "faint";
  const word = result.passed === true ? "Passed" : result.passed === false ? "Failed" : "Not run yet";
  return (
    <div className="flex flex-wrap items-center gap-2" data-testid={`check-${name}`}>
      <span className="text-base font-semibold">{name}</span>
      <StatusChip tone={tone}>{word}</StatusChip>
      {result.at ? <span className="text-xs text-ink-faint">{ago(result.at)}</span> : null}
      {result.summary && <span className="w-full text-sm text-ink-dim">{result.summary}</span>}
    </div>
  );
}

// Calibration: a list of files with the verdict you expect for each; the engine checks them
// and reports what it found.
function Calibration({ result, onDone }: { result: CheckResult; onDone: () => void }) {
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const [out, setOut] = useState<CalibrationResult | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const toast = useToast();

  const run = async () => {
    const rows = text.split("\n").map((l) => l.trim()).filter(Boolean).map((l) => {
      const i = l.lastIndexOf(",");
      return { path: (i < 0 ? l : l.slice(0, i)).trim(), expected: i < 0 ? "" : l.slice(i + 1).trim() };
    });
    if (rows.length === 0 || rows.some((r) => !r.path || !r.expected)) {
      setErr("Write one file per line as: path, expected verdict.");
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      const r = await musicEngineApi.runCalibration(rows);
      setOut(r);
      toast(r.passed ? "Calibration passed" : "Calibration failed", { tone: r.passed ? "good" : "error" });
      onDone();
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="flex flex-col gap-2">
      <CheckState name="Calibration" result={result} />
      <Field label="Files to check" hint="One per line: the file’s path, a comma, then the verdict you expect.">
        <Textarea rows={4} className="w-full px-3 py-2 font-mono text-sm" value={text} onChange={(e) => setText(e.target.value)} />
      </Field>
      <div><Button onClick={() => void run()} busy={busy} busyLabel="Running…">Run calibration</Button></div>
      {err && <div role="alert" className="text-sm" style={{ color: "var(--reject-text)" }}>{err}</div>}
      {out && (
        <ul className="m-0 flex list-none flex-col gap-1 p-0 text-sm">
          {out.rows.map((r) => (
            <li key={r.path} style={{ color: r.ok ? "var(--good-text)" : "var(--reject-text)" }}>
              {r.ok ? "Match" : "Mismatch"}: {r.path} — expected {r.expected}, got {r.actual}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

// Path test: you swap one real file by hand, see what Plex does, and record the result.
function PathTest({ result, onDone }: { result: CheckResult; onDone: () => void }) {
  const [summary, setSummary] = useState("");
  const [busy, setBusy] = useState<boolean | null>(null);
  const [err, setErr] = useState<string | null>(null);

  const record = async (passed: boolean) => {
    setBusy(passed);
    setErr(null);
    try {
      await musicEngineApi.recordPathTest(passed, summary.trim());
      onDone();
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(null);
    }
  };

  return (
    <div className="flex flex-col gap-2">
      <CheckState name="Plex path test" result={result} />
      <Field label="What you saw" hint="What Plex showed for the song before and after the swap.">
        <Textarea rows={3} className="w-full px-3 py-2 text-sm" value={summary} onChange={(e) => setSummary(e.target.value)} />
      </Field>
      <div className="flex flex-wrap gap-2">
        <Button disabled={!summary.trim() || busy !== null} busy={busy === true} onClick={() => void record(true)}>Record: it passed</Button>
        <Button disabled={!summary.trim() || busy !== null} busy={busy === false} onClick={() => void record(false)}>Record: it failed</Button>
      </div>
      {err && <div role="alert" className="text-sm" style={{ color: "var(--reject-text)" }}>{err}</div>}
      <Note tone="info">Automatic swaps are allowed only after the calibration and this test have both passed.</Note>
    </div>
  );
}
