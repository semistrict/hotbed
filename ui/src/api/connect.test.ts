import { create } from "@bufbuild/protobuf";
import { createRouterTransport } from "@connectrpc/connect";
import { describe, expect, it } from "vitest";
import {
  BaseState,
  ClusterService,
  CommandDiagnosticsSchema,
  CommandProfileSchema,
  CommandSchema,
  CommandState,
  DaemonLogLevel,
  GetCommandDiagnosticsResponseSchema,
  GetDaemonLogsResponseSchema,
  InspectCommandResponseSchema,
  InterventionKind,
  NodeSchema,
  ProcessSampleSchema,
  ScheduledCommandSchema,
  SnapshotSchema,
  WorkspaceSchema,
  WorkspaceService,
  WorkspaceState,
  baseStateLabel,
  nodeAdmission,
} from "./types";
import { ago, bytes, hhmmss, limit } from "../lib/format";
import { commandStreams, createClients } from "./connect";
import { foldStack } from "./folded";

describe("Connect clients + protobuf-es types", () => {
  it("preserves BaseState.FAILED, freshen kind, unknown arch, Command.args, ScheduledCommand.arch, and CommandLimits verbatim", async () => {
    const rawSnapshot = create(SnapshotSchema, {
      now: 1795320060000n,
      nodes: [
        create(NodeSchema, {
          id: "u",
          hostname: "ubu1",
          online: true,
          resources: { arch: "", cpus: 8, memoryBytes: 16n * 1024n ** 3n },
          admission: {
            holding: true,
            reason: "CPU pressure 72%",
            since: 1795320000000n,
            queued: 2,
            signals: [{ name: "cpu-pressure", value: 72, limit: 60, resume: 30, unit: "%", holding: true, detail: "peak 75%", waiting: 2 }],
          },
        }),
        create(NodeSchema, {
          id: "a",
          hostname: "asahimbp",
          online: false,
          resources: { arch: "arm64", cpus: 12 },
          admission: { holding: true, reason: "stale", since: 1795320000000n, queued: 1 },
        }),
      ],
      bases: [
        { id: "b-failed", project: "hotbed", nodeId: "u", generation: 3, state: BaseState.FAILED },
        { id: "b-stale", project: "hotbed", nodeId: "u", generation: 2, state: BaseState.STALE },
        { id: "b-warm", project: "hotbed", nodeId: "u", generation: 4, state: BaseState.WARM },
      ],
      workspaces: [
        create(WorkspaceSchema, {
          id: "w1",
          project: "ratel",
          baseId: "ratel-g2",
          state: WorkspaceState.IDLE,
          repos: [
            { repo: "lnx", head: "aaaa", baseCommit: "aaaa" },
            { repo: "ratel", head: "cccccccccccccc", baseCommit: "bbbb", filesChanged: 3, syncDir: "/src/ratel" },
          ],
        }),
      ],
      commands: [
        create(CommandSchema, {
          id: "cmd-freshen",
          workspaceId: "w1",
          nodeId: "u",
          argv: "cargo build --release",
          args: ["cargo", "build", "--release"],
          kind: "freshen",
          state: CommandState.RUNNING,
          queuedAt: 1795320000000n,
          limits: {
            cpuWeight: 200,
            memoryLowBytes: 2n * 1024n ** 3n,
            memoryHighBytes: 8n * 1024n ** 3n,
            softLimitSeconds: 600n,
            hardLimitSeconds: 900n,
          },
        }),
      ],
      schedules: [
        create(ScheduledCommandSchema, {
          name: "nightly-arm",
          cron: "0 2 * * *",
          repo: "hotbed",
          arch: "arm64",
          command: { args: ["go", "test", "./..."], env: ["CGO_ENABLED=0"] },
        }),
      ],
    });

    const transport = createRouterTransport(({ service }) => {
      service(ClusterService, {
        getSnapshot: () => rawSnapshot,
      });
    });

    const s = await createClients(transport).cluster.getSnapshot({});

    // BaseState.FAILED stays distinct from STALE
    expect(s.bases[0]?.state).toBe(BaseState.FAILED);
    expect(baseStateLabel(s.bases[0]!.state)).toBe("failed");
    expect(s.bases[1]?.state).toBe(BaseState.STALE);
    expect(baseStateLabel(s.bases[1]!.state)).toBe("stale");
    expect(s.bases[2]?.state).toBe(BaseState.WARM);

    // Command kind "freshen", args, and soft/hard time limits preserved verbatim
    expect(s.commands[0]?.kind).toBe("freshen");
    expect(s.commands[0]?.args).toEqual(["cargo", "build", "--release"]);
    expect(s.commands[0]?.limits?.softLimitSeconds).toBe(600n);
    expect(s.commands[0]?.limits?.hardLimitSeconds).toBe(900n);

    // Unknown arch stays empty (never coerced to "arm64")
    expect(s.nodes[0]?.resources?.arch).toBe("");
    expect(s.nodes[1]?.resources?.arch).toBe("arm64");

    // Offline node's stale admission is ignored by nodeAdmission()
    expect(nodeAdmission(s.nodes[0]!)?.holding).toBe(true);
    expect(nodeAdmission(s.nodes[1]!)).toBeUndefined();

    // ScheduledCommand.arch and command.args preserved verbatim
    expect(s.schedules[0]?.arch).toBe("arm64");
    expect(s.schedules[0]?.command?.args).toEqual(["go", "test", "./..."]);
    expect(s.schedules[0]?.command?.env).toEqual(["CGO_ENABLED=0"]);

    // Full workspace repo head is preserved verbatim
    expect(s.workspaces[0]?.repos[1]?.head).toBe("cccccccccccccc");
  });

  it("returns ProcessSample, CommandProfile, and DaemonLogs verbatim over Connect", async () => {
    const samplePb = create(ProcessSampleSchema, {
      commandId: "cmd-1",
      hostname: "ubu1",
      durationMs: 3000n,
      pollCount: 12,
      userStackSnapshots: 4,
      collectors: [{ name: "proc_delta", ok: true, detail: "1 PID" }],
      procs: [
        {
          pid: 100,
          comm: "app",
          state: "S",
          cpuPct: 12.5,
          rssBytes: 65536n,
          threads: [
            {
              tid: 100,
              name: "main",
              state: "S",
              stationary: true,
              syscall: { name: "futex", fdTarget: "" },
            },
          ],
        },
      ],
      cgroupStats: {
        cpuCoresAvg: 0.25,
        psiIoSomeDeltaUsec: 25000n,
      },
      frames: [
        { name: "app", frameType: "process" },
        { name: "main.work", module: "app", frameType: "go", sourceFile: "work.go", sourceLine: 12 },
        { name: "vfs_write", module: "vmlinux", frameType: "kernel" },
        { name: "a;b", frameType: "native" },
      ],
      sampledStacks: [{ frames: [0, 1, 2], samples: 3n, leafType: "kernel", comm: "app" }],
      onCpuStacks: [{ frames: [0, 3, 1], samples: 2n, leafType: "go", comm: "app" }],
      onCpuTopFunctions: [{ frame: 1, selfSamples: 2n, totalSamples: 5n, selfPct: 40, totalPct: 100 }],
      files: [{ pid: 100, comm: "app", fd: 5, kind: "socket", socketProto: "tcp", socketState: "ESTABLISHED", txQueueBytes: 4096n, rxQueueBytes: 128n }],
      locks: [{ kind: "FLOCK", mode: "ADVISORY", access: "WRITE", pid: 100, comm: "app", inode: 55n, range: "0..EOF", blocked: true }],
    });

    const profilePb = create(CommandProfileSchema, {
      node: "asahimbp",
      profiler: "opentelemetry-ebpf-profiler",
      live: true,
      active: true,
      sampleHz: 20,
      totalSamples: 120n,
      matchedSamples: 120n,
      userSamples: 90n,
      kernelSamples: 30n,
      comms: ["rustc", "tailscaled"],
      frameTypes: ["kernel", "native"],
      frames: [
        { name: "rustc", frameType: "process" },
        { name: "main", frameType: "native" },
      ],
      stacks: [{ frames: [0, 1], samples: 90n, leafType: "native", comm: "rustc" }],
      buckets: [{ t: 1000n, samples: 90n, topFrame: 1, durationMs: 640000n }],
    });

    const logsPb = create(GetDaemonLogsResponseSchema, {
      entries: [
        { at: 1795320000000n, level: DaemonLogLevel.ERROR, source: "controller", role: "controller", host: "ubu1", msg: "prune failed" },
        { at: 1795319900000n, level: DaemonLogLevel.WARN, source: "asahimbp", role: "worker", host: "asahimbp", msg: "volume busy" },
      ],
      sources: [
        { source: "controller", role: "controller", host: "ubu1", reachable: true, errors24h: 1, warns24h: 0, lastAt: 1795320000000n, diskBytes: 4096n, files: 2, maxFiles: 5, maxAgeDays: 14 },
      ],
      truncated: true,
    });

    const transport = createRouterTransport(({ service }) => {
      service(WorkspaceService, {
        getProfile: (req) => {
          if (req.target.case === "commandId") {
            return create(CommandProfileSchema, {
              commandId: req.target.value,
              profiler: "opentelemetry-ebpf-profiler",
              live: true,
              active: true,
              sampleHz: 20,
              totalSamples: 40n,
              matchedSamples: 40n,
            });
          }
          return profilePb;
        },
        getDaemonLogs: () => logsPb,
        async *sampleCommand() {
          yield { status: "done", progressPct: 100, sample: samplePb, done: true, kept: { commandId: "cmd-1", seq: 3 } };
        },
      });
    });

    const clients = createClients(transport);

    const final = await commandStreams(clients).sampleCommand({ id: "cmd-1", durationSeconds: 3 }, () => {});
    expect(final?.kept?.seq).toBe(3);
    const sample = final?.sample;
    expect(sample?.commandId).toBe("cmd-1");
    expect(sample?.hostname).toBe("ubu1");
    expect(sample?.durationMs).toBe(3000n);
    expect(sample?.pollCount).toBe(12);
    expect(sample?.userStackSnapshots).toBe(4);
    expect(sample?.collectors[0]?.ok).toBe(true);
    expect(sample?.procs[0]?.pid).toBe(100);
    expect(sample?.procs[0]?.threads[0]?.stationary).toBe(true);
    expect(sample?.procs[0]?.threads[0]?.syscall?.name).toBe("futex");
    expect(sample?.cgroupStats?.psiIoSomeDeltaUsec).toBe(25000n);
    expect(sample?.files[0]?.txQueueBytes).toBe(4096n);
    expect(sample?.locks[0]?.blocked).toBe(true);
    expect(sample?.sampledStacks.map((st) => foldStack(st, sample.frames))).toEqual(["app;main.work;vfs_write_[k]"]);
    expect(sample?.onCpuStacks.map((st) => foldStack(st, sample.frames))).toEqual(["app;a:b;main.work"]);
    expect(sample?.frames[sample.onCpuTopFunctions[0]!.frame]?.name).toBe("main.work");

    const cmdProf = await clients.workspaces.getProfile({ target: { case: "commandId", value: "cmd-1" } });
    expect(cmdProf.commandId).toBe("cmd-1");
    expect(cmdProf.totalSamples).toBe(40n);

    const prof = await clients.workspaces.getProfile({ target: { case: "node", value: "asahimbp" } });
    expect(prof.node).toBe("asahimbp");
    expect(prof.totalSamples).toBe(120n);
    expect(prof.stacks.map((st) => foldStack(st, prof.frames))).toEqual(["rustc;main"]);
    expect(prof.frames[prof.buckets[0]!.topFrame]?.name).toBe("main");
    expect(prof.buckets[0]?.durationMs).toBe(640000n);

    const logs = await clients.workspaces.getDaemonLogs({ level: DaemonLogLevel.WARN });
    expect(logs.truncated).toBe(true);
    expect(logs.entries[0]?.level).toBe(DaemonLogLevel.ERROR);
    expect(logs.entries[1]?.level).toBe(DaemonLogLevel.WARN);
    expect(logs.sources[0]?.diskBytes).toBe(4096n);
  });

  it("sends stopCommand, pauseCommand, resumeCommand and reads getCommandDiagnostics over Connect", async () => {
    const seen: {
      stop?: { id: string; comment: string; diagnose: boolean };
      pause?: { id: string; comment: string; diagnose: boolean };
      resume?: { id: string; comment: string };
      diagId?: string;
    } = {};

    const diagPb = create(CommandDiagnosticsSchema, {
      commandId: "cmd-hung",
      intervention: {
        seq: 1,
        kind: InterventionKind.STOP,
        by: { at: 1795320050000n, session: "", host: "ramon-mbp" },
        comment: "stuck on lock",
        diagnosed: true,
        diagnosticsError: "",
      },
      sample: create(ProcessSampleSchema, {
        commandId: "cmd-hung",
        hostname: "ubu1",
        durationMs: 3000n,
        pollCount: 10,
        procs: [{ pid: 42, comm: "cargo", state: "D" }],
      }),
      inspect: create(InspectCommandResponseSchema, {
        commandId: "cmd-hung",
        collectedAt: 1795320049000n,
        cgroup: "/hotbed/cmd-hung",
        procs: [{ pid: 42, comm: "cargo", state: "D", threadsCount: 2 }],
      }),
    });

    const transport = createRouterTransport(({ service }) => {
      service(ClusterService, {
        getCommandDiagnostics: (req) => {
          seen.diagId = req.id;
          return create(GetCommandDiagnosticsResponseSchema, { diagnostics: [diagPb] });
        },
      });
      service(WorkspaceService, {
        stopCommand: (req) => {
          seen.stop = { id: req.id, comment: req.comment, diagnose: req.diagnose };
          return create(CommandSchema, {
            id: req.id,
            state: CommandState.CANCELLED,
            interventions: [
              {
                seq: 1,
                kind: InterventionKind.STOP,
                by: { at: 1795320050000n, host: "ramon-mbp" },
                comment: req.comment,
                diagnosed: req.diagnose,
              },
            ],
          });
        },
        pauseCommand: (req) => {
          seen.pause = { id: req.id, comment: req.comment, diagnose: req.diagnose };
          return create(CommandSchema, {
            id: req.id,
            state: CommandState.PAUSED,
            interventions: [
              {
                seq: 1,
                kind: InterventionKind.PAUSE,
                by: { at: 1795320040000n, host: "ramon-mbp" },
                comment: req.comment,
                diagnosed: req.diagnose,
              },
            ],
          });
        },
        resumeCommand: (req) => {
          seen.resume = { id: req.id, comment: req.comment };
          return create(CommandSchema, {
            id: req.id,
            state: CommandState.RUNNING,
            interventions: [
              {
                seq: 2,
                kind: InterventionKind.RESUME,
                by: { at: 1795320045000n, host: "ramon-mbp" },
                comment: req.comment,
              },
            ],
          });
        },
      });
    });

    const clients = createClients(transport);

    const paused = await clients.workspaces.pauseCommand({ id: "cmd-hung", comment: "holding for debug", diagnose: true });
    expect(seen.pause).toEqual({ id: "cmd-hung", comment: "holding for debug", diagnose: true });
    expect(paused.state).toBe(CommandState.PAUSED);
    expect(paused.interventions[0]?.kind).toBe(InterventionKind.PAUSE);
    expect(paused.interventions[0]?.diagnosed).toBe(true);

    const resumed = await clients.workspaces.resumeCommand({ id: "cmd-hung", comment: "back to work" });
    expect(seen.resume).toEqual({ id: "cmd-hung", comment: "back to work" });
    expect(resumed.state).toBe(CommandState.RUNNING);
    expect(resumed.interventions[0]?.kind).toBe(InterventionKind.RESUME);

    const stopped = await clients.workspaces.stopCommand({ id: "cmd-hung", comment: "stuck on lock", diagnose: true });
    expect(seen.stop).toEqual({ id: "cmd-hung", comment: "stuck on lock", diagnose: true });
    expect(stopped.state).toBe(CommandState.CANCELLED);
    expect(stopped.interventions[0]?.kind).toBe(InterventionKind.STOP);
    expect(stopped.interventions[0]?.comment).toBe("stuck on lock");

    // Fields left out arrive as their zero values.
    await clients.workspaces.stopCommand({ id: "cmd-default" });
    expect(seen.stop).toEqual({ id: "cmd-default", comment: "", diagnose: false });
    await clients.workspaces.resumeCommand({ id: "cmd-default" });
    expect(seen.resume).toEqual({ id: "cmd-default", comment: "" });

    const diags = await commandStreams(clients).getCommandDiagnostics("cmd-hung");
    expect(seen.diagId).toBe("cmd-hung");
    expect(diags).toHaveLength(1);
    expect(diags[0]?.commandId).toBe("cmd-hung");
    expect(diags[0]?.intervention?.kind).toBe(InterventionKind.STOP);
    expect(diags[0]?.intervention?.by?.host).toBe("ramon-mbp");
    expect(diags[0]?.sample?.procs[0]?.comm).toBe("cargo");
    expect(diags[0]?.inspect?.cgroup).toBe("/hotbed/cmd-hung");
  });

  it("paginates getCommandDiagnostics across multiple pages and stops on non-advancing cursors", async () => {
    const diag1 = create(CommandDiagnosticsSchema, {
      commandId: "cmd-paged",
      intervention: { seq: 1, kind: InterventionKind.PAUSE, by: { at: 1795320040000n, host: "ramon-mbp" }, comment: "first", diagnosed: true },
    });
    const diag2 = create(CommandDiagnosticsSchema, {
      commandId: "cmd-paged",
      intervention: { seq: 2, kind: InterventionKind.STOP, by: { at: 1795320050000n, host: "ramon-mbp" }, comment: "second", diagnosed: true },
    });

    const seenAfterSeqs: number[] = [];
    const transport = createRouterTransport(({ service }) => {
      service(ClusterService, {
        getCommandDiagnostics: (req) => {
          seenAfterSeqs.push(req.afterSeq);
          if (req.id === "cmd-stalled") {
            return create(GetCommandDiagnosticsResponseSchema, {
              diagnostics: [diag1],
              hasMore: true,
              nextAfterSeq: req.afterSeq,
            });
          }
          if (req.afterSeq === 0) {
            return create(GetCommandDiagnosticsResponseSchema, {
              diagnostics: [diag1],
              hasMore: true,
              nextAfterSeq: 1,
            });
          }
          return create(GetCommandDiagnosticsResponseSchema, {
            diagnostics: [diag2],
            hasMore: false,
            nextAfterSeq: 0,
          });
        },
      });
    });

    const { getCommandDiagnostics } = commandStreams(createClients(transport));
    const combined = await getCommandDiagnostics("cmd-paged");
    expect(seenAfterSeqs).toEqual([0, 1]);
    expect(combined.map((d) => d.intervention?.seq)).toEqual([1, 2]);
    expect(combined.map((d) => d.intervention?.comment)).toEqual(["first", "second"]);

    // Non-advancing cursor must terminate cleanly instead of looping.
    seenAfterSeqs.length = 0;
    const stalled = await getCommandDiagnostics("cmd-stalled");
    expect(seenAfterSeqs).toEqual([0]);
    expect(stalled).toHaveLength(1);
  });

  it("calls ClusterService.retireNode with the target node and returns the retired Node", async () => {
    let seenNode = "";
    const transport = createRouterTransport(({ service }) => {
      service(ClusterService, {
        retireNode: (req) => {
          seenNode = req.node;
          return create(NodeSchema, {
            id: "node-dead-1",
            hostname: "old-box",
            online: false,
          });
        },
      });
    });

    const retired = await createClients(transport).cluster.retireNode({ node: "node-dead-1" });
    expect(seenNode).toBe("node-dead-1");
    expect(retired.id).toBe("node-dead-1");
    expect(retired.hostname).toBe("old-box");
    expect(retired.online).toBe(false);
  });

  it("formats bigint timestamps and byte sizes directly without ISO string round-trips", () => {
    expect(ago(1795320000000n, 1795320030000n)).toBe("30s ago");
    expect(ago(1795320000000n, 1795320000000n)).toBe("now");
    expect(bytes(4n * 1024n ** 3n)).toBe("4 GiB");
    expect(limit(0n)).toBe("none");
    expect(limit(2n * 1024n ** 3n)).toBe("2 GiB");
    expect(hhmmss(1795320000000n)).toMatch(/^\d{2}:\d{2}:\d{2}$/);
  });
});
