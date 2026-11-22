import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";
import { BaseState, CommandState, RunOutcome, SnapshotSchema, WorkspaceState } from "../gen/hotbed/v1/hotbed_pb";
import { toSnapshot } from "./connect";

describe("toSnapshot", () => {
  it("maps the wire's bigints, enums and unset fields onto the cockpit's types", () => {
    const wire = create(SnapshotSchema, {
      clusterId: "c0ffee",
      controller: "ubu1",
      now: 1795300000000n,
      nodes: [
        {
          id: "nAsahi",
          hostname: "asahimbp",
          online: true,
          joinedAt: 1795200000000n,
          resources: { arch: "arm64", cpus: 11, memoryBytes: 19327352832n, machine: "MacBook Pro (14-inch, M3 Pro, Nov 2023)", benchCpus: "7-10" },
          usage: { cpuBusy: 0.25, memoryUsedBytes: 4294967296n },
          profiler: { active: true, engine: "opentelemetry-ebpf-profiler", sampleHz: 20, totalSamples: 1234n },
        },
      ],
      workspaces: [
        { id: "p1", state: WorkspaceState.PREPARING, reason: "waiting for base app-g2 to freshen" },
        { id: "f1", state: WorkspaceState.FAILED, reason: "freshen exited 3" },
        { id: "i1", state: WorkspaceState.IDLE },
      ],
      bases: [{ id: "app-g1", repoId: "app", state: BaseState.WARM, generation: 1, builtAt: 1795250000000n }],
      commands: [
        {
          id: "k3m9x2qa",
          state: CommandState.PARKED,
          kind: "test",
          queuedAt: 1795299000000n,
          reason: "frozen at 94% memory in use",
          timeline: [{ at: 1795299000000n, what: "queued", detail: "by claude" }],
        },
        { id: "b3nch000", state: CommandState.RUNNING, kind: "shell", bench: true, queuedAt: 1795299000000n },
      ],
      history: [
        { id: "a", state: CommandState.FAILED, failure: "test", exitCode: 100, coldSeconds: 370, endedAt: 1795299500000n, seconds: 14.2 },
        { id: "b", state: CommandState.SUCCEEDED, bench: true, endedAt: 1795299500000n, seconds: 3 },
      ],
      series: { nAsahi: { samples: [{ t: 1795299995000n, cpuBusy: 0.25, memoryUsed: 0.22 }] } },
      sessions: [
        {
          id: "brave-otter",
          agent: "laptop",
          startedAt: 1795299000000n,
          lastActiveAt: 1795299900000n,
          workspaces: [{ id: "p1", repoId: "app", nodeId: "nAsahi", createdAt: 1795299000000n, releasedAt: 1795299500000n }],
        },
        { id: "nightly-20261121-0200", agent: "schedule:nightly", schedule: "nightly", scheduleRunning: true, startedAt: 1795251600000n, lastActiveAt: 1795251600000n },
      ],
      schedules: [
        {
          name: "nightly",
          cron: "0 2 * * *",
          repo: "app",
          node: "ubu1",
          command: { args: ["sh", "-c", "echo it's late", "plain"], env: ["WHO=nightly"] },
          createdAt: 1795200000000n,
          updatedAt: 1795200000000n,
          nextAt: 1795338000000n,
          runs: [
            {
              id: 2n,
              schedule: "nightly",
              slotAt: 1795251600000n,
              startedAt: 1795251600000n,
              outcome: RunOutcome.FAILED,
              sessionId: "nightly-20261121-0200",
              commandId: "k3m9x2qa",
              nodeId: "nUbu1",
              exitCode: 3,
              reason: "exited 3",
              endedAt: 1795255200000n,
            },
            { id: 1n, schedule: "nightly", slotAt: 1795165200000n, startedAt: 1795165500000n, endedAt: 1795165500000n, outcome: RunOutcome.MISSED, reason: "the controller was down" },
          ],
        },
        { name: "soak", cron: "0 * * * *", repo: "app", paused: true, command: { args: ["true"] } },
      ],
    });
    const s = toSnapshot(wire);
    expect(s.now).toBe("2026-11-21T22:26:40.000Z");
    expect(s.nodes[0]).toMatchObject({ hostname: "asahimbp", arch: "arm64", cpus: 11, memoryBytes: 19327352832, lastSeen: "2026-11-20T18:40:00.000Z" });
    expect(s.nodes[0]?.usage.memoryUsedBytes).toBe(4294967296);
    expect(s.nodes[0]?.benchCpus).toBe("7-10");
    expect(s.nodes[0]?.profiler).toEqual({ active: true, engine: "opentelemetry-ebpf-profiler", sampleHz: 20, totalSamples: 1234, error: "" });
    expect(s.commands.map((c) => [c.id, c.bench])).toEqual([
      ["k3m9x2qa", false],
      ["b3nch000", true],
    ]);
    expect(s.history.map((c) => [c.id, c.bench])).toEqual([
      ["a", false],
      ["b", true],
    ]);
    expect(s.bases[0]).toMatchObject({ state: "warm", builtAt: "2026-11-21T08:33:20.000Z" });
    expect(s.commands[0]).toMatchObject({ state: "parked", kind: "test", startedAt: null, exitCode: null, url: null, reason: "frozen at 94% memory in use" });
    expect(s.history[0]).toMatchObject({ state: "failed", failure: "test", exitCode: 100, coldSeconds: 370, rebuilt: null });
    expect(s.workspaces.map((w) => [w.id, w.state, w.reason])).toEqual([
      ["p1", "preparing", "waiting for base app-g2 to freshen"],
      ["f1", "failed", "freshen exited 3"],
      ["i1", "idle", ""],
    ]);
    expect(s.sessions).toEqual([
      {
        id: "brave-otter",
        agent: "laptop",
        startedAt: "2026-11-21T22:10:00.000Z",
        lastActiveAt: "2026-11-21T22:25:00.000Z",
        endedAt: null,
        endReason: "",
        workspaces: [{ id: "p1", repoId: "app", nodeId: "nAsahi", createdAt: "2026-11-21T22:10:00.000Z", releasedAt: "2026-11-21T22:18:20.000Z" }],
        schedule: null,
        scheduleRunning: false,
      },
      {
        id: "nightly-20261121-0200",
        agent: "schedule:nightly",
        startedAt: "2026-11-21T09:00:00.000Z",
        lastActiveAt: "2026-11-21T09:00:00.000Z",
        endedAt: null,
        endReason: "",
        workspaces: [],
        schedule: "nightly",
        scheduleRunning: true,
      },
    ]);
    expect(s.schedules).toEqual([
      {
        name: "nightly",
        cron: "0 2 * * *",
        repoId: "app",
        node: "ubu1",
        argv: "sh -c 'echo it'\\''s late' plain",
        env: ["WHO=nightly"],
        paused: false,
        createdAt: "2026-11-20T18:40:00.000Z",
        updatedAt: "2026-11-20T18:40:00.000Z",
        nextAt: "2026-11-22T09:00:00.000Z",
        runs: [
          {
            id: 2,
            schedule: "nightly",
            slotAt: "2026-11-21T09:00:00.000Z",
            startedAt: "2026-11-21T09:00:00.000Z",
            endedAt: "2026-11-21T10:00:00.000Z",
            outcome: "failed",
            sessionId: "nightly-20261121-0200",
            commandId: "k3m9x2qa",
            nodeId: "nUbu1",
            exitCode: 3,
            reason: "exited 3",
          },
          {
            id: 1,
            schedule: "nightly",
            slotAt: "2026-11-20T09:00:00.000Z",
            startedAt: "2026-11-20T09:05:00.000Z",
            endedAt: "2026-11-20T09:05:00.000Z",
            outcome: "missed",
            sessionId: null,
            commandId: null,
            nodeId: null,
            exitCode: null,
            reason: "the controller was down",
          },
        ],
      },
      {
        name: "soak",
        cron: "0 * * * *",
        repoId: "app",
        node: null,
        argv: "true",
        env: [],
        paused: true,
        createdAt: "1970-01-01T00:00:00.000Z",
        updatedAt: "1970-01-01T00:00:00.000Z",
        nextAt: null,
        runs: [],
      },
    ]);
    expect(s.series.nAsahi).toEqual([{ t: 1795299995000, cpuBusy: 0.25, memoryUsed: 0.22, memoryPressure: 0 }]);
  });
});
