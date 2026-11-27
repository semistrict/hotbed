import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import {
  clients,
  CommandState,
  getCommandDiagnostics,
  InterventionKind,
  type Command,
  type CommandDiagnostics,
  type CommandIntervention,
} from "../api";
import { hhmmss } from "../lib/format";
import { isLive } from "../lib/live";
import { InspectReport } from "./LiveDiagnosticsPanel";
import { Card } from "./Panel";
import { SampleReport } from "./SampleReport";
import { SessionLink } from "./SessionLink";

export type ByHandAction = "start" | "stop" | "pause" | "resume";

const ACTS: Record<ByHandAction, { label: string; glyph: string; doing: string; diagnosing: string; hint: string }> = {
  start: {
    label: "Start now",
    glyph: "▶",
    doing: "Starting…",
    diagnosing: "Starting…",
    hint: "Starts it now, past what holds work on its node: memory, CPU and disk. It still waits for its workspace and its VM to be free.",
  },
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
  if (c.state === CommandState.QUEUED) return c.bench ? ["stop"] : ["start", "stop"];
  if (c.bench) return ["stop"];
  return ["pause", "stop"];
}

/**
 * Start, stop, pause and resume buttons for a live command, each opening a form for
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
        <Button
          key={act}
          type="button"
          variant={act === "stop" ? "destructive" : "outline"}
          size="sm"
          aria-expanded={open === act}
          onClick={() => onOpen(open === act ? null : act)}
          title={ACTS[act].hint}
          className={`gap-1.5 font-mono text-[11.5px] font-semibold ${
            act === "stop"
              ? `border-critical/50 text-critical ${open === act ? "bg-destructive/20 ring-1 ring-critical/40" : ""}`
              : `border-border-strong text-foreground ${open === act ? "bg-muted" : ""}`
          }`}
        >
          <span aria-hidden>{ACTS[act].glyph}</span>
          {ACTS[act].label}
        </Button>
      ))}
    </>
  );
}

/** Whether act can take a command's diagnostics first: a stop or pause of one that has started. */
function takesDiagnosticsFor(act: ByHandAction, state: CommandState): boolean {
  return (act === "stop" || act === "pause") && state !== CommandState.QUEUED;
}

/** Effective diagnostics toggle: follows command state until the user explicitly overrides it. */
export function effectiveDiagnose(act: ByHandAction, state: CommandState, override: boolean | null): boolean {
  const takesDiagnostics = takesDiagnosticsFor(act, state);
  return takesDiagnostics && (override ?? true);
}

/** The form a start, stop, pause or resume button opens: a comment, and diagnostics first. */
export function ByHandForm({ command, act, onDone }: { command: Command; act: ByHandAction; onDone: () => void }) {
  const qc = useQueryClient();
  const [comment, setComment] = useState("");
  const takesDiagnostics = takesDiagnosticsFor(act, command.state);
  const [diagnoseOverride, setDiagnoseOverride] = useState<boolean | null>(null);
  const diagnose = effectiveDiagnose(act, command.state, diagnoseOverride);
  const send = useMutation({
    mutationFn: () => {
      const trimmed = comment.trim();
      switch (act) {
        case "stop":
          return clients.workspaces.stopCommand({ id: command.id, comment: trimmed, diagnose: takesDiagnostics && diagnose });
        case "pause":
          return clients.workspaces.pauseCommand({ id: command.id, comment: trimmed, diagnose: takesDiagnostics && diagnose });
        case "resume":
          return clients.workspaces.resumeCommand({ id: command.id, comment: trimmed });
        case "start":
          return clients.workspaces.startCommand({ id: command.id, comment: trimmed });
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
      <Textarea
        rows={2}
        value={comment}
        autoFocus
        disabled={send.isPending}
        onChange={(e) => setComment(e.target.value)}
        placeholder={
          act === "resume" || act === "start"
            ? `Why ${act}? Recorded on the command (optional)`
            : "Why stop or pause? Shown to the agent waiting on this command"
        }
        aria-label={`Comment on the ${act}`}
        className="min-h-14 bg-background/60 text-[12.5px] placeholder:text-faint"
      />
      <div className="flex flex-wrap items-center gap-3">
        {takesDiagnostics && (
          <Label className="inline-flex cursor-pointer items-center gap-2 text-[12px] font-normal text-foreground">
            <Checkbox
              checked={diagnose}
              disabled={send.isPending}
              onCheckedChange={(checked) => setDiagnoseOverride(Boolean(checked))}
            />
            <span>
              Take diagnostics first <span className="text-faint">(activity sample & inspection, ~3s)</span>
            </span>
          </Label>
        )}
        <div className="ml-auto flex items-center gap-2">
          <Button type="button" variant="ghost" size="sm" onClick={onDone} disabled={send.isPending} className="text-[12px] text-muted-foreground">
            Cancel
          </Button>
          <Button
            type="submit"
            variant={act === "stop" ? "destructive" : "default"}
            size="sm"
            disabled={send.isPending}
            className={act === "stop" ? "bg-destructive text-destructive-foreground hover:bg-destructive/90 dark:bg-destructive dark:hover:bg-destructive/90" : undefined}
          >
            {send.isPending && <span className="live-dot inline-block size-2 rounded-full bg-current" aria-hidden />}
            {send.isPending ? (takesDiagnostics && diagnose ? a.diagnosing : a.doing) : a.label}
          </Button>
        </div>
      </div>
      {send.error && (
        <Alert variant="destructive" className="border-critical/30 font-mono text-[11px]">
          <AlertDescription className="text-[11px] text-critical">
            {send.error instanceof Error ? send.error.message : String(send.error)}
          </AlertDescription>
        </Alert>
      )}
    </form>
  );
}

const VERB: Record<InterventionKind, { label: string; glyph: string; tone: string }> = {
  [InterventionKind.UNSPECIFIED]: { label: "Intervened", glyph: "•", tone: "text-foreground" },
  [InterventionKind.STOP]: { label: "Stopped", glyph: "■", tone: "text-critical" },
  [InterventionKind.PAUSE]: { label: "Paused", glyph: "⏸", tone: "text-frost" },
  [InterventionKind.RESUME]: { label: "Resumed", glyph: "▶", tone: "text-primary" },
  [InterventionKind.START]: { label: "Started", glyph: "▶", tone: "text-primary" },
};

/**
 * A command's starts, stops, pauses and resumes by hand, each with what it was doing
 * then: its diagnostics, which the controller keeps after it ends.
 * `interventions` are the live command's; an ended one's come from its saved
 * diagnostics.
 */
export function InterventionsCard({
  id,
  interventions,
}: {
  id: string;
  interventions: CommandIntervention[] | null;
}) {
  const needsDiagnostics = interventions === null || interventions.some((iv) => iv.diagnosed);
  const taken = useQuery({
    queryKey: ["diagnostics", id, interventions?.length ?? -1],
    queryFn: () => getCommandDiagnostics(id),
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
      title="Started, stopped & paused by hand"
      sub="Each with what the command was doing then; the agent waiting on it was told who, why, and how to read this."
    >
      {shown.length > 0 && (
        <ol className="space-y-3">
          {shown.map((iv) => (
            <InterventionRow
              key={iv.seq}
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
        <Alert
          variant="destructive"
          className={`${shown.length > 0 ? "mt-2" : ""} flex flex-wrap items-center justify-between gap-2 border-critical/30 bg-[color-mix(in_oklab,var(--critical)_10%,transparent)] px-3 py-2 font-mono text-[11px] text-critical`}
        >
          <AlertDescription className="text-[11px] text-critical">
            Could not load saved diagnostics: {taken.error instanceof Error ? taken.error.message : String(taken.error)}
          </AlertDescription>
          <Button
            type="button"
            variant="outline"
            size="xs"
            onClick={() => void taken.refetch()}
            disabled={taken.isFetching}
            className="border-critical/50 font-mono text-[10.5px] font-semibold text-foreground"
          >
            {taken.isFetching ? "Retrying…" : "Retry"}
          </Button>
        </Alert>
      )}
    </Card>
  );
}

export function InterventionRow({
  iv,
  diagnostics,
  loading,
  defaultOpen = null,
}: {
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
              <SessionLink id={session} />
              {host && <span className="text-faint"> on {host}</span>}
            </>
          ) : host !== "" ? (
            <span className="font-mono text-[11px] text-foreground">{host}</span>
          ) : (
            <span className="text-foreground">cockpit</span>
          )}
        </span>
        {at > 0n && (
          <span className="ml-auto font-mono text-[10.5px] text-faint" title={new Date(Number(at)).toLocaleString()}>
            {hhmmss(at)}
          </span>
        )}
      </div>
      {iv.comment && (
        <blockquote className="mt-2 border-l-2 border-primary/50 pl-3 text-[12.5px] whitespace-pre-wrap text-foreground">
          {iv.comment}
        </blockquote>
      )}
      {iv.diagnosticsError && (
        <p className="mt-1.5 font-mono text-[11px] text-warning-ink">
          {iv.diagnosed ? `Not all diagnostics taken: ${iv.diagnosticsError}` : `No diagnostics: ${iv.diagnosticsError}`}
        </p>
      )}
      {iv.diagnosed && (
        <div className="mt-2.5 space-y-3">
          <div className="flex flex-wrap items-center gap-1.5 font-mono text-[11px]">
            {diagnostics?.sample && (
              <Button
                type="button"
                variant="outline"
                size="xs"
                aria-expanded={open === "sample"}
                onClick={() => setOpen(open === "sample" ? null : "sample")}
                className={
                  open === "sample"
                    ? "border-primary bg-ember-wash font-mono text-[11px] text-primary"
                    : "font-mono text-[11px] text-muted-foreground"
                }
              >
                Activity sample
              </Button>
            )}
            {diagnostics?.inspect && (
              <Button
                type="button"
                variant="outline"
                size="xs"
                aria-expanded={open === "inspect"}
                onClick={() => setOpen(open === "inspect" ? null : "inspect")}
                className={
                  open === "inspect"
                    ? "border-primary bg-ember-wash font-mono text-[11px] text-primary"
                    : "font-mono text-[11px] text-muted-foreground"
                }
              >
                Inspection
              </Button>
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
