import { clone } from "@bufbuild/protobuf";
import { type QueryClient, useQuery, useQueryClient } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { api, type CommandSample, type ProcessSample } from "../api";
import { CommandSampleSchema } from "../gen/hotbed/v1/hotbed_pb";
import { hhmmss } from "../lib/format";
import type { Taking } from "../lib/sampling";
import { Card } from "./Panel";
import { SampleReport } from "./SampleReport";

export const sampleKeys = {
  list: (id: string) => ["command-samples", id] as const,
  one: (id: string, seq: number) => ["command-sample", id, seq] as const,
};

function newestSeq(samples: readonly CommandSample[] | undefined): number {
  return samples?.reduce((n, s) => Math.max(n, s.seq), 0) ?? 0;
}

/**
 * A fresh listing of a command's samples, with those known here that are
 * newer than its newest: a sample just kept reaches the controller a moment
 * after its taker has it, and a listing must not drop it meanwhile.
 */
export function withKnown(listed: readonly CommandSample[], known: readonly CommandSample[] | undefined): CommandSample[] {
  const newest = newestSeq(listed);
  return [...listed, ...(known ?? []).filter((k) => k.seq > newest)];
}

/**
 * The samples kept of command id, oldest first, without their contents.
 * count, for a live command, is how many it has: the list is fetched again
 * until it holds them all, so a sample anyone takes shows up.
 */
export function useCommandSamples(id: string, count: number | undefined) {
  const qc = useQueryClient();
  return useQuery({
    queryKey: sampleKeys.list(id),
    queryFn: async () => withKnown(await api.listCommandSamples(id), qc.getQueryData<CommandSample[]>(sampleKeys.list(id))),
    staleTime: Infinity,
    refetchInterval: (q) => (count !== undefined && newestSeq(q.state.data) < count ? 1000 : false),
  });
}

/** Files a sample just kept, so its tab opens on it without asking again. */
export function seedKeptSample(qc: QueryClient, id: string, kept: CommandSample, sample: ProcessSample) {
  const full = clone(CommandSampleSchema, kept);
  full.sample = sample;
  qc.setQueryData(sampleKeys.one(id, kept.seq), full);
  qc.setQueryData(sampleKeys.list(id), (old: CommandSample[] | undefined) => withKnown(old ?? [], [kept]));
}

/** What a kept sample was: when, how long, which PID, and who took it. */
export function sampleSummary(s: CommandSample): string {
  const who = s.by?.session ? `session ${s.by.session}` : s.by?.host;
  return [
    s.by?.at ? hhmmss(s.by.at) : null,
    `${s.durationSeconds}s window`,
    s.pid ? `PID ${s.pid}` : "all processes",
    who ? `by ${who}` : null,
  ]
    .filter(Boolean)
    .join(" · ");
}

/** One kept sample of command id, in full. onSamplePid, while it runs, samples one of its processes. */
export function KeptSampleView({ id, seq, onSamplePid }: { id: string; seq: number; onSamplePid?: (pid: number) => void }) {
  const q = useQuery({ queryKey: sampleKeys.one(id, seq), queryFn: () => api.getCommandSample(id, seq), staleTime: Infinity });
  const s = q.data;
  return (
    <Card title={`Sample #${seq}`} sub={s ? sampleSummary(s) : q.isError ? "Unavailable" : "Loading"}>
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
