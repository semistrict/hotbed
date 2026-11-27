import { create } from "@bufbuild/protobuf";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import type { ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";
import {
  CommandDiagnosticsSchema,
  CommandInterventionSchema,
  CommandSchema,
  CommandState,
  InspectCommandResponseSchema,
  InterventionKind,
  ProcessSampleSchema,
} from "../api";
import { hhmmss } from "../lib/format";

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, params }: { children?: ReactNode; params?: { id: string } }) => <a href={`/sessions/${params?.id}`}>{children}</a>,
  useNavigate: () => vi.fn(),
}));
import { actsFor, ByHandButtons, ByHandForm, effectiveDiagnose, InterventionRow, InterventionsCard } from "./ByHand";

function renderWithQuery(node: React.ReactNode, qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })) {
  return renderToStaticMarkup(<QueryClientProvider client={qc}>{node}</QueryClientProvider>);
}

describe("ByHand controls", () => {
  it("computes allowed actions per command state", () => {
    expect(actsFor({ state: CommandState.RUNNING, bench: false })).toEqual(["pause", "stop"]);
    expect(actsFor({ state: CommandState.PARKED, bench: false })).toEqual(["pause", "stop"]);
    expect(actsFor({ state: CommandState.QUEUED, bench: false })).toEqual(["stop"]);
    expect(actsFor({ state: CommandState.RUNNING, bench: true })).toEqual(["stop"]);
    expect(actsFor({ state: CommandState.PAUSED, bench: false })).toEqual(["resume", "stop"]);
    expect(actsFor({ state: CommandState.SUCCEEDED, bench: false })).toEqual([]);
    expect(actsFor({ state: CommandState.FAILED, bench: false })).toEqual([]);
    expect(actsFor({ state: CommandState.CANCELLED, bench: false })).toEqual([]);
  });

  it("renders Pause and Stop buttons when running, and Resume and Stop when paused", () => {
    const running = create(CommandSchema, { id: "cmd-run", state: CommandState.RUNNING });
    const runningHtml = renderToStaticMarkup(<ByHandButtons command={running} open={null} onOpen={() => {}} />);
    expect(runningHtml).toContain("Pause");
    expect(runningHtml).toContain("Stop");
    expect(runningHtml).not.toContain("Resume");

    const paused = create(CommandSchema, { id: "cmd-pause", state: CommandState.PAUSED });
    const pausedHtml = renderToStaticMarkup(<ByHandButtons command={paused} open={null} onOpen={() => {}} />);
    expect(pausedHtml).toContain("Resume");
    expect(pausedHtml).toContain("Stop");
    expect(pausedHtml).not.toContain("Pause");

    const queued = create(CommandSchema, { id: "cmd-q", state: CommandState.QUEUED });
    const queuedHtml = renderToStaticMarkup(<ByHandButtons command={queued} open={null} onOpen={() => {}} />);
    expect(queuedHtml).toContain("Stop");
    expect(queuedHtml).not.toContain("Pause");
    expect(queuedHtml).not.toContain("Resume");
  });

  it("renders ByHandForm with comment input and default-checked diagnostics checkbox for stop/pause on running commands", () => {
    const running = create(CommandSchema, { id: "cmd-run", state: CommandState.RUNNING });
    const stopHtml = renderWithQuery(<ByHandForm command={running} act="stop" onDone={() => {}} />);
    expect(stopHtml).toContain("Why stop or pause? Shown to the agent waiting on this command");
    expect(stopHtml).toContain("Take diagnostics first");
    expect(stopHtml).toMatch(/<input[^>]*type="checkbox"[^>]*checked=""/);

    const pauseHtml = renderWithQuery(<ByHandForm command={running} act="pause" onDone={() => {}} />);
    expect(pauseHtml).toContain("Take diagnostics first");
    expect(pauseHtml).toMatch(/<input[^>]*type="checkbox"[^>]*checked=""/);

    const queued = create(CommandSchema, { id: "cmd-q", state: CommandState.QUEUED });
    const queuedStopHtml = renderWithQuery(<ByHandForm command={queued} act="stop" onDone={() => {}} />);
    expect(queuedStopHtml).not.toContain("Take diagnostics first");

    const paused = create(CommandSchema, { id: "cmd-pause", state: CommandState.PAUSED });
    const resumeHtml = renderWithQuery(<ByHandForm command={paused} act="resume" onDone={() => {}} />);
    expect(resumeHtml).not.toContain("Take diagnostics first");
    expect(resumeHtml).toContain("Resume");
  });

  it("uses solid destructive styling for Stop confirmation and keeps active Stop trigger non-muted", () => {
    const running = create(CommandSchema, { id: "cmd-run", state: CommandState.RUNNING });
    const stopFormHtml = renderWithQuery(<ByHandForm command={running} act="stop" onDone={() => {}} />);
    expect(stopFormHtml).toContain("bg-destructive");
    expect(stopFormHtml).toContain("text-destructive-foreground");

    const openStopButtonsHtml = renderToStaticMarkup(<ByHandButtons command={running} open="stop" onOpen={() => {}} />);
    const stopBtnMatch = openStopButtonsHtml.match(/<button[^>]*aria-expanded="true"[^>]*>/);
    expect(stopBtnMatch?.[0]).toBeTruthy();
    expect(stopBtnMatch?.[0]).not.toContain("bg-muted");
    expect(stopBtnMatch?.[0]).toContain("bg-destructive/20");

    const openPauseButtonsHtml = renderToStaticMarkup(<ByHandButtons command={running} open="pause" onOpen={() => {}} />);
    const pauseBtnMatch = openPauseButtonsHtml.match(/<button[^>]*aria-expanded="true"[^>]*>/);
    expect(pauseBtnMatch?.[0]).toContain("bg-muted");
  });

  it("syncs diagnostics default when command transitions from QUEUED to RUNNING unless user explicitly overrode it", () => {
    expect(effectiveDiagnose("stop", CommandState.QUEUED, null)).toBe(false);
    expect(effectiveDiagnose("stop", CommandState.RUNNING, null)).toBe(true);
    expect(effectiveDiagnose("pause", CommandState.RUNNING, null)).toBe(true);
    expect(effectiveDiagnose("stop", CommandState.RUNNING, false)).toBe(false);
    expect(effectiveDiagnose("stop", CommandState.QUEUED, true)).toBe(false);
    expect(effectiveDiagnose("resume", CommandState.RUNNING, null)).toBe(false);
    expect(effectiveDiagnose("resume", CommandState.RUNNING, true)).toBe(false);
  });
});

describe("InterventionsCard & saved diagnostics", () => {
  const atMs = 1795320050000n;

  const sample = create(ProcessSampleSchema, {
    commandId: "cmd-1",
    hostname: "ubu1",
    arch: "amd64",
    endedAt: atMs,
    durationMs: 3000n,
    pollCount: 12,
    userStackSnapshots: 4,
    collectors: [{ name: "proc_delta", ok: true, detail: "1 PID" }],
    procs: [
      {
        pid: 412,
        ppid: 1,
        comm: "cargo-nextest",
        state: "S",
        cpuPct: 4.5,
        rssBytes: 1024n * 1024n,
        threads: [
          {
            tid: 412,
            name: "main",
            state: "S",
            stationary: true,
            pollsSameStack: 12,
            totalPolls: 12,
            syscall: { name: "futex", fdTarget: "" },
          },
        ],
      },
    ],
    frames: [
      { name: "cargo-nextest", frameType: "process" },
      { name: "std::sync::Mutex::lock", module: "cargo-nextest", frameType: "native" },
    ],
    sampledStacks: [{ frames: [0, 1], samples: 12n, leafType: "native", comm: "cargo-nextest" }],
  });

  const inspect = create(InspectCommandResponseSchema, {
    commandId: "cmd-1",
    collectedAt: atMs,
    cgroup: "/hotbed/cmd-1",
    procs: [
      {
        pid: 412,
        ppid: 1,
        comm: "cargo-nextest",
        cmdline: "cargo nextest run",
        state: "S",
        threadsCount: 3,
        rssBytes: 2048n * 1024n,
        wchan: "futex_wait_queue",
      },
    ],
  });

  it("renders intervention metadata, comment, diagnosticsError, and saved SampleReport + InspectReport cleanly", () => {
    const iv = create(CommandInterventionSchema, {
      seq: 1,
      kind: InterventionKind.STOP,
      by: { at: atMs, session: "", host: "ramon-mbp" },
      comment: "Hung for 20 minutes on eviction test",
      diagnosed: true,
      diagnosticsError: "perf unavailable",
    });
    const diag = create(CommandDiagnosticsSchema, {
      commandId: "cmd-1",
      intervention: iv,
      sample,
      inspect,
    });

    const sampleOpenHtml = renderToStaticMarkup(
      <ol>
        <InterventionRow iv={iv} diagnostics={diag} loading={false} defaultOpen="sample" />
      </ol>,
    );
    expect(sampleOpenHtml).toContain("Stopped");
    expect(sampleOpenHtml).toContain("ramon-mbp");
    expect(sampleOpenHtml).not.toContain(">you<");
    expect(sampleOpenHtml).not.toContain("on ramon-mbp");
    expect(sampleOpenHtml).toContain(hhmmss(atMs));
    expect(sampleOpenHtml).toContain(`title="${new Date(Number(atMs)).toLocaleString()}"`);
    expect(sampleOpenHtml).toContain("Hung for 20 minutes on eviction test");
    expect(sampleOpenHtml).toContain("Not all diagnostics taken: perf unavailable");
    expect(sampleOpenHtml).toContain("text-warning-ink");
    expect(sampleOpenHtml).toContain("Activity sample");
    expect(sampleOpenHtml).toContain("Inspection");
    expect(sampleOpenHtml).toContain("cargo-nextest");
    expect(sampleOpenHtml).toContain("std::sync::Mutex::lock");
    expect(sampleOpenHtml).not.toContain("Sample PID");

    const inspectOpenHtml = renderToStaticMarkup(
      <ol>
        <InterventionRow iv={iv} diagnostics={diag} loading={false} defaultOpen="inspect" />
      </ol>,
    );
    expect(inspectOpenHtml).toContain("/hotbed/cmd-1");
    expect(inspectOpenHtml).toContain("cargo nextest run");
    expect(inspectOpenHtml).toContain("futex_wait_queue");
  });

  it("renders agent session attribution, partial/uncollected diagnostics states, and ended-command saved interventions", () => {
    const pauseIv = create(CommandInterventionSchema, {
      seq: 1,
      kind: InterventionKind.PAUSE,
      by: { at: atMs, session: "brave-otter", host: "ubu1" },
      comment: "pausing while profiling another build",
      diagnosed: true,
    });
    const resumeIv = create(CommandInterventionSchema, {
      seq: 2,
      kind: InterventionKind.RESUME,
      by: { at: atMs + 5000n, session: "brave-otter", host: "ubu1" },
      comment: "resuming now",
      diagnosed: false,
      diagnosticsError: "",
    });
    const failedDiagIv = create(CommandInterventionSchema, {
      seq: 3,
      kind: InterventionKind.STOP,
      by: { at: atMs + 10000n, host: "laptop" },
      diagnosed: false,
      diagnosticsError: "worker unreachable",
    });

    const loadingHtml = renderToStaticMarkup(
      <ol>
        <InterventionRow iv={pauseIv} diagnostics={null} loading={true} />
        <InterventionRow iv={resumeIv} diagnostics={null} loading={false} />
        <InterventionRow iv={failedDiagIv} diagnostics={null} loading={false} />
      </ol>,
    );
    expect(loadingHtml).toContain("Paused");
    expect(loadingHtml).toContain('<a href="/sessions/brave-otter">brave-otter</a>');
    expect(loadingHtml).toContain("Loading its diagnostics…");
    expect(loadingHtml).toContain("Resumed");
    expect(loadingHtml).toContain("resuming now");
    expect(loadingHtml).toContain("No diagnostics: worker unreachable");

    // Ended command (`interventions={null}`) reads its interventions and diagnostics from cached getCommandDiagnostics.
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    qc.setQueryData(
      ["diagnostics", "cmd-1", -1],
      [create(CommandDiagnosticsSchema, { commandId: "cmd-1", intervention: pauseIv, sample, inspect })],
    );
    const endedHtml = renderWithQuery(<InterventionsCard id="cmd-1" interventions={null} />, qc);
    expect(endedHtml).toContain("Stopped &amp; paused by hand");
    expect(endedHtml).toContain("pausing while profiling another build");
    expect(endedHtml).toContain("Activity sample");
    expect(endedHtml).toContain("Inspection");
  });

  it("attributes sessionless interventions to their recorded host or cockpit instead of 'you'", () => {
    const hostOnlyIv = create(CommandInterventionSchema, {
      seq: 1,
      kind: InterventionKind.STOP,
      by: { at: atMs, session: "", host: "workstation-1" },
    });
    const hostlessIv = create(CommandInterventionSchema, {
      seq: 2,
      kind: InterventionKind.PAUSE,
      by: { at: atMs, session: "", host: "" },
    });

    const hostOnlyHtml = renderToStaticMarkup(
      <ol>
        <InterventionRow iv={hostOnlyIv} diagnostics={null} loading={false} />
      </ol>,
    );
    expect(hostOnlyHtml).toContain("by ");
    expect(hostOnlyHtml).toContain("workstation-1");
    expect(hostOnlyHtml).not.toContain("you");
    expect(hostOnlyHtml).not.toContain("on workstation-1");

    const hostlessHtml = renderToStaticMarkup(
      <ol>
        <InterventionRow iv={hostlessIv} diagnostics={null} loading={false} />
      </ol>,
    );
    expect(hostlessHtml).toContain("cockpit");
    expect(hostlessHtml).not.toContain("you");
  });

  it("renders error and loading states before returning null when saved diagnostics fetch fails or is pending", async () => {
    // Archived command (`interventions={null}`) with a failed getCommandDiagnostics query must show the error, not null.
    const failedQc = new QueryClient({ defaultOptions: { queries: { retry: false, retryOnMount: false } } });
    await failedQc
      .fetchQuery({
        queryKey: ["diagnostics", "cmd-archived", -1],
        queryFn: () => Promise.reject(new Error("controller unavailable")),
      })
      .catch(() => {});

    const failedArchivedHtml = renderWithQuery(
      <InterventionsCard id="cmd-archived" interventions={null} />,
      failedQc,
    );
    expect(failedArchivedHtml).toContain("Stopped &amp; paused by hand");
    expect(failedArchivedHtml).toContain('role="alert"');
    expect(failedArchivedHtml).toContain("controller unavailable");
    expect(failedArchivedHtml).toContain("Retry");
    const retryBtnMatch = failedArchivedHtml.match(/<button[^>]*>Retry<\/button>/);
    expect(retryBtnMatch?.[0]).toBeTruthy();
    expect(retryBtnMatch?.[0]).not.toContain("bg-destructive");
    expect(retryBtnMatch?.[0]).toContain("bg-background");

    // Empty interventions array with a failed diagnostics query also surfaces the error instead of returning null.
    const failedEmptyListQc = new QueryClient({ defaultOptions: { queries: { retry: false, retryOnMount: false } } });
    await failedEmptyListQc
      .fetchQuery({
        queryKey: ["diagnostics", "cmd-empty", 0],
        queryFn: () => Promise.reject(new Error("RPC timeout")),
      })
      .catch(() => {});

    const failedEmptyListHtml = renderWithQuery(
      <InterventionsCard id="cmd-empty" interventions={[]} />,
      failedEmptyListQc,
    );
    expect(failedEmptyListHtml).toContain('role="alert"');
    expect(failedEmptyListHtml).toContain("RPC timeout");
    expect(failedEmptyListHtml).toContain("Retry");

    // Archived command while pending renders loading state; once resolved empty, returns null.
    const pendingQc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const pendingHtml = renderWithQuery(
      <InterventionsCard id="cmd-loading" interventions={null} />,
      pendingQc,
    );
    expect(pendingHtml).toContain("Loading saved diagnostics");

    const emptyResolvedQc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    emptyResolvedQc.setQueryData(["diagnostics", "cmd-none", -1], []);
    expect(renderWithQuery(<InterventionsCard id="cmd-none" interventions={null} />, emptyResolvedQc)).toBe("");

    // Live command with no interventions and no error still returns null.
    expect(renderWithQuery(<InterventionsCard id="cmd-live-clean" interventions={[]} />)).toBe("");
  });
});
