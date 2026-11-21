import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";
import { BaseState, CommandState, SnapshotSchema, WorkspaceState } from "../gen/hotbed/v1/hotbed_pb";
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
          resources: { arch: "arm64", cpus: 11, memoryBytes: 19327352832n, machine: "MacBook Pro (14-inch, M3 Pro, Nov 2023)" },
          usage: { cpuBusy: 0.25, memoryUsedBytes: 4294967296n },
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
      ],
      history: [{ id: "a", state: CommandState.FAILED, failure: "test", exitCode: 100, coldSeconds: 370, endedAt: 1795299500000n, seconds: 14.2 }],
      series: { nAsahi: { samples: [{ t: 1795299995000n, cpuBusy: 0.25, memoryUsed: 0.22 }] } },
    });
    const s = toSnapshot(wire);
    expect(s.now).toBe("2026-11-21T22:26:40.000Z");
    expect(s.nodes[0]).toMatchObject({ hostname: "asahimbp", arch: "arm64", cpus: 11, memoryBytes: 19327352832, lastSeen: "2026-11-20T18:40:00.000Z" });
    expect(s.nodes[0]?.usage.memoryUsedBytes).toBe(4294967296);
    expect(s.bases[0]).toMatchObject({ state: "warm", builtAt: "2026-11-21T08:33:20.000Z" });
    expect(s.commands[0]).toMatchObject({ state: "parked", kind: "test", startedAt: null, exitCode: null, url: null, reason: "frozen at 94% memory in use" });
    expect(s.history[0]).toMatchObject({ state: "failed", failure: "test", exitCode: 100, coldSeconds: 370, rebuilt: null });
    expect(s.workspaces.map((w) => [w.id, w.state, w.reason])).toEqual([
      ["p1", "preparing", "waiting for base app-g2 to freshen"],
      ["f1", "failed", "freshen exited 3"],
      ["i1", "idle", ""],
    ]);
    expect(s.series.nAsahi).toEqual([{ t: 1795299995000, cpuBusy: 0.25, memoryUsed: 0.22, memoryPressure: 0 }]);
  });
});
