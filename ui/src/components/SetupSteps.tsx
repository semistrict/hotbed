import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { api, StepState, stepStateLabel, type Project, type StepStatus } from "../api";
import { ago, duration } from "../lib/format";
import { ScriptBlock } from "./CommandBlock";

const STATE_TONE: Record<StepState, string> = {
  [StepState.UNSPECIFIED]: "border-warning/50 text-warning",
  [StepState.APPLIED]: "border-sprout/40 text-sprout",
  [StepState.PENDING]: "border-warning/50 text-warning",
  [StepState.FAILED]: "border-critical/50 text-critical",
};

const STATE_MARK: Record<StepState, string> = {
  [StepState.UNSPECIFIED]: "◌",
  [StepState.APPLIED]: "✓",
  [StepState.PENDING]: "◌",
  [StepState.FAILED]: "✕",
};

/**
 * A project's setup steps: the bash scripts every base of it runs, in order,
 * in /work beside its repos, with how each node's newest base stands with
 * each. The steps come with the snapshot; where each node stands is asked for
 * while they are shown.
 */
export function SetupSteps({ project, now }: { project: Project; now: number | bigint }) {
  const [open, setOpen] = useState(true);
  const status = useQuery({
    queryKey: ["steps", project.name],
    queryFn: () => api.steps(project.name),
    enabled: open && project.steps.length > 0,
    refetchInterval: 5000,
  });
  const nodes = new Map<string, StepStatus["nodes"]>((status.data ?? []).filter((s) => s.step).map((s) => [s.step!.name, s.nodes]));
  const pending = (status.data ?? []).reduce((n, s) => n + s.nodes.filter((x) => x.state !== StepState.APPLIED).length, 0);

  return (
    <section aria-label={`${project.name} setup steps`} className="rounded-xl border border-border/80 bg-background/35">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        aria-expanded={open}
        className="flex w-full flex-wrap items-baseline gap-x-3 gap-y-1 px-4 py-3 text-left"
      >
        <span className="font-mono text-[10.5px] text-faint">{open ? "▾" : "▸"}</span>
        <span className="text-[12.5px] font-semibold text-foreground">Setup steps</span>
        <span className="font-mono text-[11px] text-muted-foreground">
          {project.steps.length === 0 ? "none" : `${project.steps.length} · run in order in /work, beside every repo`}
        </span>
        {pending > 0 && <span className="font-mono text-[11px] text-warning">{pending} not yet in a node's newest base</span>}
        {project.hosts.length > 0 && <span className="ml-auto font-mono text-[11px] text-faint">bases kept on {project.hosts.join(", ")}</span>}
      </button>
      {open && (
        <div className="space-y-3 border-t border-border/60 px-4 pt-3 pb-4">
          {project.steps.length === 0 ? (
            <p className="font-mono text-[11.5px] text-faint">
              No setup steps: workspaces start with the checkouts alone. hotbed step add -p {project.name} NAME -- SCRIPT tries one in a workspace and saves it.
            </p>
          ) : (
            <ol className="space-y-4">
              {project.steps.map((step, i) => (
                <li key={step.name} className="space-y-1.5">
                  <div className="flex flex-wrap items-baseline gap-x-2.5 gap-y-1 font-mono text-[11px]">
                    <span className="text-faint">{i + 1}.</span>
                    <span className="text-[12.5px] font-medium text-foreground">{step.name}</span>
                    {step.everyPull && (
                      <span className="rounded border border-frost/40 px-1.5 py-px text-[10px] text-frost" title="Runs again whenever any repo of the project moved">
                        every pull
                      </span>
                    )}
                    <span className="text-faint" title="The script's sha256: a base that ran another runs it again">
                      {step.hash}
                    </span>
                    <span className="text-faint">
                      set {ago(step.updatedAt, now)}
                      {step.updatedBy && ` from ${step.updatedBy}`}
                    </span>
                    <span className="ml-auto flex flex-wrap gap-1.5">
                      {(nodes.get(step.name) ?? []).map((n) => (
                        <span
                          key={n.node}
                          className={`rounded border px-1.5 py-px text-[10px] ${STATE_TONE[n.state]}`}
                          title={n.baseId ? `${n.node}'s newest base, ${n.baseId}: ${stepStateLabel(n.state)}` : `${n.node} has no base yet`}
                        >
                          {STATE_MARK[n.state]} {n.node}
                          {n.state === StepState.APPLIED && n.seconds > 0 && <span className="text-faint"> {duration(n.seconds)}</span>}
                        </span>
                      ))}
                    </span>
                  </div>
                  <ScriptBlock script={step.script} />
                </li>
              ))}
            </ol>
          )}
          {status.isError && <p className="font-mono text-[11px] text-critical">Cannot read where each node stands: {String(status.error)}</p>}
        </div>
      )}
    </section>
  );
}
