import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import {
  api,
  CommandState,
  InterventionKind,
  type Command,
  type CommandDiagnostics,
  type CommandIntervention,
  type Snapshot,
} from "../api";
import { hhmmss } from "../lib/format";
import { isLive } from "../lib/live";
import { InspectReport } from "./LiveDiagnosticsPanel";
import { SampleReport } from "./ProcessSamplePanel";
import { SessionLink } from "./SessionLink";
import { Card } from "./ui/Card";

export type ByHandAction = "stop" | "pause" | "resume";

const ACTS: Record<ByHandAction, { label: string; glyph: string; doing: string; diagnosing: string; hint: string }> = {
  stop: {
    label: "Stop",
    glyph: "■",
    doing: "Stopping…",
    diagnosing: "Taking diagnostics, then stopping…",
    hint: "Ends it. The agent waiting on it is told who stopped it, your comment, and how to read the diagnostics.",
  },
  pause: {
    label: "Pause",
    glyph: "⏸",
    doing: "Pausing…",
    diagnosing: "Taking diagnostics, then pausing…",
    hint: "Freezes it, its memory to swap. The agent waiting on it is told so; it stays paused until you resume it.",
  },
  resume: {
    label: "Resume",
    glyph: "▶",
    doing: "Resuming…",
    diagnosing: "Resuming…",
    hint: "Thaws it; it runs on, under its limits.",
  },
};

/** What can be done by hand to a command as it is now. */
export function actsFor(c: Pick<Command, "state" | "bench">): ByHandAction[] {
  if (!isLive(c)) return [];
  if (c.state === CommandState.PAUSED) return ["resume", "stop"];
  if (c.state === CommandState.QUEUED || c.bench) return ["stop"];
  return ["pause", "stop"];
}

/**
 * Stop, pause and resume buttons for a live command, each opening a form for
 * the comment its waiting agent gets and whether to take its diagnostics first.
 */
export function ByHandButtons({
  command,
  open,
  onOpen,
}: {
  command: Command;
  open: ByHandAction | null;
  onOpen: (act: ByHandAction | null) => void;
}) {
  return (
    <>
      {actsFor(command).map((act) => (
        <button
          key={act}
          type="button"
          aria-expanded={open === act}
          onClick={() => onOpen(open === act ? null : act)}
          title={ACTS[act].hint}
          className={`inline-flex items-center gap-1.5 rounded-lg border px-3 py-1.5 font-mono text-[11.5px] font-semibold transition-colors ${
            act === "stop"
              ? "border-critical/50 text-critical hover:bg-[color-mix(in_oklab,var(--critical)_12%,transparent)]"
              : "border-border-strong text-foreground hover:bg-muted"
          } ${open === act ? "bg-muted" : ""}`}
        >
          <span aria-hidden>{ACTS[act].glyph}</span>
          {ACTS[act].label}
        </button>
      ))}
    </>
  );
}

/** The form a stop, pause or resume button opens: a comment, and diagnostics first. */
export function ByHandForm({ command, act, onDone }: { command: Command; act: ByHandAction; onDone: () => void }) {
  const qc = useQueryClient();
  const [comment, setComment] = useState("");
  const takesDiagnostics = act !== "resume" && command.state !== CommandState.QUEUED;
  const [diagnose, setDiagnose] = useState(command.state !== CommandState.QUEUED);
  const send = useMutation({
    mutationFn: () => {
      const trimmed = comment.trim();
      switch (act) {
        case "stop":
          return api.stopCommand(command.id, { comment: trimmed, diagnose: takesDiagnostics && diagnose });
        case "pause":
          return api.pauseCommand(command.id, { comment: trimmed, diagnose: takesDiagnostics && diagnose });
        case "resume":
          return api.resumeCommand(command.id, { comment: trimmed });
      }
    },
    onSuccess: onDone,
    onSettled: () =>
      Promise.all([
        qc.invalidateQueries({ queryKey: ["snapshot"] }),
        qc.invalidateQueries({ queryKey: ["diagnostics", command.id] }),
      ]),
  });
  const a = ACTS[act];
  return (
    <form
      className="rise mt-3 space-y-2.5 rounded-xl border border-border-strong bg-card/80 p-3.5"
      onSubmit={(e) => {
        e.preventDefault();
        send.mutate();
      }}
    >
      <p className="text-[12px] text-muted-foreground">{a.hint}</p>
      <textarea
        rows={2}
        value={comment}
        autoFocus
        disabled={send.isPending}
        onChange={(e) => setComment(e.target.value)}
        placeholder={
          act === "resume"
            ? "Why resume? Recorded on the command (optional)"
            : "Why stop or pause? Shown to the agent waiting on this command"
        }
        aria-label={`Comment on the ${act}`}
        className="w-full rounded-lg border border-border bg-background/60 px-3 py-2 text-[12.5px] placeholder:text-faint focus:border-primary/50 focus:ring-2 focus:ring-primary/20 focus:outline-none"
      />
      <div className="flex flex-wrap items-center gap-3">
        {takesDiagnostics && (
          <label className="inline-flex cursor-pointer items-center gap-2 text-[12px] text-foreground">
            <input
              type="checkbox"
              checked={diagnose}
              disabled={send.isPending}
              onChange={(e) => setDiagnose(e.target.checked)}
              className="accent-primary"
            />
            Take diagnostics first <span className="text-faint">(activity sample & inspection, ~3s)</span>
          </label>
        )}
        <div className="ml-auto flex items-center gap-2">
          <button
            type="button"
            onClick={onDone}
            disabled={send.isPending}
            className="rounded-lg px-3 py-1.5 text-[12px] text-muted-foreground hover:text-foreground disabled:opacity-40"
          >
            Cancel
          </button>
          <button
            type="submit"
            disabled={send.isPending}
            className={`inline-flex items-center gap-1.5 rounded-lg px-3.5 py-1.5 text-[12.5px] font-medium transition-opacity hover:opacity-90 disabled:opacity-60 ${
              act === "stop"
                ? "border border-critical bg-[color-mix(in_oklab,var(--critical)_18%,transparent)] text-critical"
                : "bg-primary text-primary-foreground"
            }`}
          >
            {send.isPending && <span className="live-dot inline-block size-2 rounded-full bg-current" aria-hidden />}
            {send.isPending ? (takesDiagnostics && diagnose ? a.diagnosing : a.doing) : a.label}
          </button>
        </div>
      </div>
      {send.error && (
        <p role="alert" className="font-mono text-[11px] text-critical">
          {send.error instanceof Error ? send.error.message : String(send.error)}
        </p>
      )}
    </form>
  );
}

const VERB: Record<InterventionKind, { label: string; glyph: string; tone: string }> = {
  [InterventionKind.UNSPECIFIED]: { label: "Intervened", glyph: "•", tone: "text-foreground" },
  [InterventionKind.STOP]: { label: "Stopped", glyph: "■", tone: "text-critical" },
  [InterventionKind.PAUSE]: { label: "Paused", glyph: "⏸", tone: "text-frost" },
  [InterventionKind.RESUME]: { label: "Resumed", glyph: "▶", tone: "text-primary" },
};

/**
 * A command's stops, pauses and resumes by hand, each with what it was doing
 * then: its diagnostics, which the controller keeps after it ends.
 * `interventions` are the live command's; an ended one's come from its saved
 * diagnostics.
 */
export function InterventionsCard({
  s,
  id,
  interventions,
}: {
  s: Snapshot;
  id: string;
  interventions: CommandIntervention[] | null;
}) {
  const needsDiagnostics = interventions === null || interventions.some((iv) => iv.diagnosed);
  const taken = useQuery({
    queryKey: ["diagnostics", id, interventions?.length ?? -1],
    queryFn: () => api.getCommandDiagnostics(id),
    enabled: needsDiagnostics,
  });
  const diagnostics = taken.data ?? [];
  const shown: CommandIntervention[] =
    interventions ?? diagnostics.flatMap((d) => (d.intervention ? [d.intervention] : []));
  const hasError = Boolean(taken.error);
  const loadingEmpty = needsDiagnostics && taken.isPending && !hasError;
  if (shown.length === 0 && !hasError && !loadingEmpty) return null;
  return (
    <Card
      title="Stopped & paused by hand"
      sub="Each with what the command was doing then; the agent waiting on it was told who, why, and how to read this."
    >
      {shown.length > 0 && (
        <ol className="space-y-3">
          {shown.map((iv) => (
            <InterventionRow
              key={iv.seq}
              s={s}
              iv={iv}
              diagnostics={diagnostics.find((d) => d.intervention?.seq === iv.seq) ?? null}
              loading={taken.isPending}
            />
          ))}
        </ol>
      )}
      {shown.length === 0 && loadingEmpty && (
        <p className="font-mono text-[11.5px] text-muted-foreground">Loading saved diagnostics…</p>
      )}
      {hasError && (
        <div
          role="alert"
          className={`${shown.length > 0 ? "mt-2" : ""} flex flex-wrap items-center justify-between gap-2 rounded-lg border border-critical/30 bg-[color-mix(in_oklab,var(--critical)_10%,transparent)] px-3 py-2 font-mono text-[11px] text-critical`}
        >
          <span>
            Could not load saved diagnostics: {taken.error instanceof Error ? taken.error.message : String(taken.error)}
          </span>
          <button
            type="button"
            onClick={() => void taken.refetch()}
            disabled={taken.isFetching}
            className="rounded-md border border-critical/50 px-2.5 py-0.5 text-[10.5px] font-semibold text-critical hover:bg-[color-mix(in_oklab,var(--critical)_15%,transparent)] disabled:opacity-50"
          >
            {taken.isFetching ? "Retrying…" : "Retry"}
          </button>
        </div>
      )}
    </Card>
  );
}

export function InterventionRow({
  s,
  iv,
  diagnostics,
  loading,
  defaultOpen = null,
}: {
  s: Snapshot;
  iv: CommandIntervention;
  diagnostics: CommandDiagnostics | null;
  loading: boolean;
  defaultOpen?: "sample" | "inspect" | null;
}) {
  const [open, setOpen] = useState<"sample" | "inspect" | null>(defaultOpen);
  const verb = VERB[iv.kind] ?? VERB[InterventionKind.UNSPECIFIED];
  const session = iv.by?.session ?? "";
  const host = iv.by?.host ?? "";
  const at = iv.by?.at ?? 0n;
  const listed = session !== "" && s.sessions.some((x) => x.id === session);

  return (
    <li className="rounded-xl border border-border bg-background/40 p-3.5">
      <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1 text-[12.5px]">
        <span className={`inline-flex items-center gap-1 font-semibold ${verb.tone}`}>
          <span aria-hidden>{verb.glyph}</span>
          {verb.label}
        </span>
        <span className="text-muted-foreground">
          by{" "}
          {session !== "" ? (
            <>
              {listed ? (
                <SessionLink id={session} />
              ) : (
                <span className="font-mono text-[11px] text-foreground">{session}</span>
              )}
              {host && <span className="text-faint"> on {host}</span>}
            </>
          ) : host !== "" ? (
            <span className="font-mono text-[11px] text-foreground">{host}</span>
          ) : (
            <span className="text-foreground">cockpit</span>
          )}
        </span>
        {at > 0n && <span className="ml-auto font-mono text-[10.5px] text-faint">{hhmmss(at)}</span>}
      </div>
      {iv.comment && (
        <blockquote className="mt-2 border-l-2 border-primary/50 pl-3 text-[12.5px] whitespace-pre-wrap text-foreground">
          {iv.comment}
        </blockquote>
      )}
      {iv.diagnosticsError && (
        <p className="mt-1.5 font-mono text-[11px] text-warning">
          {iv.diagnosed ? `Not all diagnostics taken: ${iv.diagnosticsError}` : `No diagnostics: ${iv.diagnosticsError}`}
        </p>
      )}
      {iv.diagnosed && (
        <div className="mt-2.5 space-y-3">
          <div className="flex flex-wrap items-center gap-1.5 font-mono text-[11px]">
            {diagnostics?.sample && (
              <button
                type="button"
                aria-expanded={open === "sample"}
                onClick={() => setOpen(open === "sample" ? null : "sample")}
                className={`rounded-lg border px-3 py-1 transition-colors ${
                  open === "sample"
                    ? "border-primary bg-ember-wash text-primary"
                    : "border-border text-muted-foreground hover:text-foreground"
                }`}
              >
                Activity sample
              </button>
            )}
            {diagnostics?.inspect && (
              <button
                type="button"
                aria-expanded={open === "inspect"}
                onClick={() => setOpen(open === "inspect" ? null : "inspect")}
                className={`rounded-lg border px-3 py-1 transition-colors ${
                  open === "inspect"
                    ? "border-primary bg-ember-wash text-primary"
                    : "border-border text-muted-foreground hover:text-foreground"
                }`}
              >
                Inspection
              </button>
            )}
            {!diagnostics && (
              <span className="text-faint">
                {loading ? "Loading its diagnostics…" : "Its diagnostics have not reached the controller yet."}
              </span>
            )}
          </div>
          {open === "sample" && diagnostics?.sample && <SampleReport sample={diagnostics.sample} />}
          {open === "inspect" && diagnostics?.inspect && <InspectReport result={diagnostics.inspect} />}
        </div>
      )}
    </li>
  );
}
