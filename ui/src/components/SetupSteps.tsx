import { useQuery } from "@tanstack/react-query";
import { ChevronRightIcon } from "lucide-react";
import { useState } from "react";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { api, StepState, stepStateLabel, type Project, type StepStatus } from "../api";
import { ago, duration } from "../lib/format";
import { ScriptBlock } from "./CommandBlock";

const STATE_TONE: Record<StepState, string> = {
  [StepState.UNSPECIFIED]: "border-warning/50 text-warning-ink",
  [StepState.APPLIED]: "border-good/40 text-good-ink",
  [StepState.PENDING]: "border-warning/50 text-warning-ink",
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
 * each. Defaults collapsed and auto-opens only when a step is FAILED or PENDING
 * (or when `defaultOpen` is requested).
 */
export function SetupSteps({
  project,
  now,
  defaultOpen = false,
}: {
  project: Project;
  now: number | bigint;
  defaultOpen?: boolean;
}) {
  const [userOpen, setUserOpen] = useState<boolean | null>(defaultOpen ? true : null);
  const hasSteps = project.steps.length > 0;
  const status = useQuery({
    queryKey: ["steps", project.name],
    queryFn: () => api.steps(project.name),
    enabled: hasSteps,
    refetchInterval: 5000,
  });
  const nodes = new Map<string, StepStatus["nodes"]>((status.data ?? []).filter((s) => s.step).map((s) => [s.step!.name, s.nodes]));
  const failed = (status.data ?? []).reduce((n, s) => n + s.nodes.filter((x) => x.state === StepState.FAILED).length, 0);
  const pending = (status.data ?? []).reduce((n, s) => n + s.nodes.filter((x) => x.state === StepState.PENDING).length, 0);
  const open = userOpen ?? (hasSteps && (failed > 0 || pending > 0));

  return (
    <Collapsible
      open={open}
      onOpenChange={setUserOpen}
      aria-label={`${project.name} setup steps`}
      className={hasSteps ? "rounded-xl border border-border/80 bg-background/35" : "rounded-lg border border-border/50 bg-background/20"}
    >
      <CollapsibleTrigger
        className={`group flex w-full cursor-pointer flex-wrap items-center gap-x-2.5 gap-y-1 text-left ${
          hasSteps ? "px-3 py-2.5 sm:px-4 sm:py-3" : "px-3 py-1.5 sm:px-3.5 sm:py-2"
        }`}
      >
        <ChevronRightIcon className="size-3.5 shrink-0 text-faint transition-transform group-data-panel-open:rotate-90" />
        <span className={`font-semibold ${hasSteps ? "text-[12.5px] text-foreground" : "text-[11.5px] text-muted-foreground"}`}>Setup steps</span>
        <span className="font-mono text-[11px] text-muted-foreground">
          {!hasSteps ? "none" : `${project.steps.length} · run in order in /work`}
        </span>
        {failed > 0 && <span className="font-mono text-[11px] text-critical">{failed} failed</span>}
        {pending > 0 && <span className="font-mono text-[11px] text-warning-ink">{pending} pending</span>}
        {project.hosts.length > 0 && <span className="font-mono text-[11px] text-faint sm:ml-auto">bases on {project.hosts.join(", ")}</span>}
      </CollapsibleTrigger>
      <CollapsibleContent>
        <div className="space-y-3 border-t border-border/60 px-3 pt-3 pb-3.5 sm:px-4 sm:pb-4">
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
                    <span className="flex flex-wrap gap-1.5 sm:ml-auto">
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
      </CollapsibleContent>
    </Collapsible>
  );
}
