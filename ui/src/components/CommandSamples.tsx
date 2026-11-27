import { clone } from "@bufbuild/protobuf";
import { type QueryClient, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Button } from "@/components/ui/button";
import { clients, CommandSampleSchema, type CommandSample, type ProcessSample } from "../api";
import { hhmmss } from "../lib/format";
import type { Taking } from "../lib/sampling";
import { Card } from "./Panel";
import { SampleReport } from "./SampleReport";
import { SessionLink } from "./SessionLink";

export const sampleKeys = {
  list: (id: string) => ["command-samples", id] as const,
  one: (id: string, seq: number) => ["command-sample", id, seq] as const,
};

function newestSeq(samples: readonly CommandSample[] | undefined): number {
  return samples?.reduce((n, s) => Math.max(n, s.seq), 0) ?? 0;
}

/**
 * A command's samples as known here: those the controller listed, and those
 * kept here that it has not listed yet (a sample reaches the controller a
 * moment after its taker has it).
 */
export interface SampleList {
  listed: CommandSample[];
  seeded: CommandSample[];
}

/** The samples to show, oldest first: all listed, and those seeded the listing lacks. */
export function samplesShown(l: SampleList): CommandSample[] {
  const listed = new Set(l.listed.map((s) => s.seq));
  return [...l.listed, ...l.seeded.filter((s) => !listed.has(s.seq))].sort((a, b) => a.seq - b.seq);
}

/** A fresh listing: seeds it now holds are the controller's, the rest wait on. */
export function relisted(listed: CommandSample[], prev: SampleList | undefined): SampleList {
  const have = new Set(listed.map((s) => s.seq));
  return { listed, seeded: (prev?.seeded ?? []).filter((s) => !have.has(s.seq)) };
}

/**
 * How long after a command's sample count changes its list is asked for
 * again, waiting for the controller to list the newest: a sample reaches it
 * within seconds of being kept, and one pruned (kept 30 days) never will.
 */
export const SAMPLE_DELIVERY_WINDOW_MS = 60_000;

/** Whether to ask for a command's samples again: one counted is not listed, and it may still come. */
export function awaitingSamples(listed: readonly CommandSample[] | undefined, count: number | undefined, countChangedAt: number, now: number): boolean {
  return count !== undefined && newestSeq(listed) < count && now - countChangedAt < SAMPLE_DELIVERY_WINDOW_MS;
}

/**
 * The samples kept of command id, oldest first, without their contents.
 * count is how many it has (Command.samples, FinishedCommand.samples): the
 * list is fetched again while the controller has yet to list the newest, so
 * a sample anyone takes, or one still on its way as the command ends, shows
 * up.
 */
export function useCommandSamples(id: string, count: number | undefined) {
  const qc = useQueryClient();
  // When count last changed, set as it renders, so the very render that
  // brings a new count asks again.
  const [counted, setCounted] = useState(() => ({ count, at: Date.now() }));
  if (counted.count !== count) setCounted({ count, at: Date.now() });
  return useQuery({
    queryKey: sampleKeys.list(id),
    queryFn: async () => relisted((await clients.cluster.listCommandSamples({ id })).samples, qc.getQueryData<SampleList>(sampleKeys.list(id))),
    staleTime: Infinity,
    refetchInterval: (q) => (awaitingSamples(q.state.data?.listed, count, counted.at, Date.now()) ? 1000 : false),
    select: samplesShown,
  });
}

/** Files a sample just kept, so its tab opens on it without asking again. */
export function seedKeptSample(qc: QueryClient, id: string, kept: CommandSample, sample: ProcessSample) {
  const full = clone(CommandSampleSchema, kept);
  full.sample = sample;
  qc.setQueryData(sampleKeys.one(id, kept.seq), full);
  qc.setQueryData(sampleKeys.list(id), (old: SampleList | undefined) => ({
    listed: old?.listed ?? [],
    seeded: [...(old?.seeded ?? []).filter((s) => s.seq !== kept.seq), kept],
  }));
}

/** What a kept sample was: when, how long, which PID, and who took it. */
function SampleSummary({ s }: { s: CommandSample }) {
  const what = [s.by?.at ? hhmmss(s.by.at) : null, `${s.durationSeconds}s window`, s.pid ? `PID ${s.pid}` : "all processes"].filter(Boolean).join(" · ");
  return (
    <>
      {what}
      {s.by?.session ? (
        <>
          {" · by "}
          <SessionLink id={s.by.session} />
          {s.by.host && ` on ${s.by.host}`}
        </>
      ) : (
        s.by?.host && ` · by ${s.by.host}`
      )}
    </>
  );
}

/** One kept sample of command id, in full. onSamplePid, while it runs, samples one of its processes. */
export function KeptSampleView({ id, seq, onSamplePid }: { id: string; seq: number; onSamplePid?: (pid: number) => void }) {
  const q = useQuery({ queryKey: sampleKeys.one(id, seq), queryFn: () => clients.cluster.getCommandSample({ id, seq }), staleTime: Infinity });
  const s = q.data;
  return (
    <Card title={`Sample #${seq}`} sub={s ? <SampleSummary s={s} /> : q.isError ? "Unavailable" : "Loading"}>
      {q.isError ? (
        <Notice tone="critical">Cannot load sample #{seq}: {String(q.error)}</Notice>
      ) : !s ? (
        <div className="font-mono text-[11.5px] text-faint">loading sample…</div>
      ) : (
        <div className="space-y-3">
          {s.truncated && <Notice tone="warning">Kept trimmed: {s.truncated}.</Notice>}
          {s.sample ? <SampleReport sample={s.sample} onSamplePid={onSamplePid} /> : <Notice tone="critical">This sample holds nothing.</Notice>}
        </div>
      )}
    </Card>
  );
}

/** A sample being taken, or one that ended without being kept. */
export function TakingView({ taking, onCancel, onDismiss, onSamplePid }: { taking: Taking; onCancel: () => void; onDismiss: () => void; onSamplePid?: (pid: number) => void }) {
  const title = taking.done ? (taking.error ? "Sample failed" : taking.cancelled ? "Sample cancelled" : "Sample not kept") : "Sampling";
  const sub = `${taking.durationSeconds}s window · ${taking.pid ? `PID ${taking.pid}` : "all processes"}`;
  return (
    <Card
      title={title}
      sub={sub}
      right={
        taking.done ? (
          <Button type="button" variant="outline" size="sm" onClick={onDismiss} className="font-mono text-[11px]">
            Close
          </Button>
        ) : (
          <Button type="button" variant="destructive" size="sm" onClick={onCancel} className="font-mono text-[11px] font-semibold">
            Cancel
          </Button>
        )
      }
    >
      <div className="space-y-4">
        {!taking.done && (
          <div className="space-y-2 rounded-xl border border-primary/30 bg-ember-wash/60 px-3.5 py-3">
            <div className="flex items-center justify-between gap-2 font-mono text-[11.5px]">
              <span className="inline-flex items-center gap-2 text-foreground">
                <span className="live-dot inline-block size-2 rounded-full bg-primary" />
                {taking.status || "Sampling…"}
              </span>
              <span className="num text-primary">{taking.progress}%</span>
            </div>
            <div className="h-1.5 w-full overflow-hidden rounded-full bg-background/70">
              <div className="h-full rounded-full bg-primary transition-[width] duration-200" style={{ width: `${Math.max(5, Math.min(100, taking.progress))}%` }} />
            </div>
          </div>
        )}
        {taking.error && <Notice tone="critical">{taking.error}</Notice>}
        {taking.done && taking.notKept && <Notice tone="warning">It could not be kept: {taking.notKept}. It is shown here until you close it.</Notice>}
        {taking.done && taking.sample && <SampleReport sample={taking.sample} onSamplePid={onSamplePid} />}
      </div>
    </Card>
  );
}

function Notice({ tone, children }: { tone: "critical" | "warning"; children: React.ReactNode }) {
  const cls =
    tone === "critical"
      ? "border-critical/30 bg-[color-mix(in_oklab,var(--critical)_10%,transparent)] text-critical"
      : "border-warning/30 bg-[color-mix(in_oklab,var(--warning)_10%,transparent)] text-warning-ink";
  return <div className={`rounded-lg border px-3 py-2 font-mono text-[11.5px] ${cls}`}>{children}</div>;
}
