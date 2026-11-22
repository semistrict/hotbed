import { createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import {
  BaseState,
  ClusterService,
  CommandState,
  RunOutcome,
  WorkspaceService,
  WorkspaceState,
  type Base as PbBase,
  type Command as PbCommand,
  type FinishedCommand as PbFinished,
  type Node as PbNode,
  type ScheduledCommand as PbScheduled,
  type ScheduledRun as PbRun,
  type Snapshot as PbSnapshot,
  type Workspace as PbWorkspace,
} from "../gen/hotbed/v1/hotbed_pb";
import type {
  Api,
  Base,
  ClusterEvent,
  Command,
  FailureKind,
  FinishedCommand,
  Node,
  ScheduledCommand,
  ScheduledRun,
  Session,
  Snapshot,
  Workspace,
} from "./types";

/**
 * The controller's API over Connect, from the page's own origin: the
 * controller serves this UI and the API side by side.
 */
export function createConnectApi(baseUrl = window.location.origin): Api {
  const transport = createConnectTransport({ baseUrl });
  const cluster = createClient(ClusterService, transport);
  const workspaces = createClient(WorkspaceService, transport);
  return {
    snapshot: async () => toSnapshot(await cluster.getSnapshot({})),
    commandLog: async (id) => {
      const log = await workspaces.getLogLines({ id, tail: 2000 });
      return { commandId: log.commandId, lines: log.lines };
    },
    files: async (workspaceId) => {
      const res = await workspaces.listFiles({ workspaceId });
      return res.files.map((f) => ({
        path: f.path,
        kind: f.kind === "dir" ? "dir" : "file",
        sizeBytes: num(f.sizeBytes),
        change: f.change === "added" || f.change === "modified" ? f.change : null,
      }));
    },
    file: async (workspaceId, path) => {
      const f = await workspaces.getFile({ workspaceId, path });
      return { path: f.path, text: f.text, changedLines: f.changedLines };
    },
  };
}

// The cockpit's view types predate the wire types; everything below maps
// one onto the other: bigints to numbers, enums to words, unset to null.

const num = (n: bigint | number | undefined): number => (n === undefined ? 0 : Number(n));
const iso = (ms: bigint | number | undefined): string => new Date(num(ms)).toISOString();
const isoOrNull = (ms: bigint | undefined): string | null => (ms === undefined ? null : iso(ms));
const orNull = <T>(v: T | undefined | ""): T | null => (v === undefined || v === "" ? null : v);

export function toSnapshot(s: PbSnapshot): Snapshot {
  return {
    clusterId: s.clusterId,
    controller: s.controller,
    now: iso(s.now),
    sampleSeconds: s.sampleSeconds,
    nodes: s.nodes.map(toNode),
    repos: s.repos.map((r) => ({
      id: r.id,
      name: r.name,
      language: r.language === "go" || r.language === "rust" ? r.language : "typescript",
      units: r.units,
      coldBuildSeconds: r.coldBuildSeconds,
    })),
    bases: s.bases.map(toBase),
    workspaces: s.workspaces.map(toWorkspace),
    commands: s.commands.map(toCommand),
    events: s.events.map(
      (e): ClusterEvent => ({
        id: num(e.id),
        at: iso(e.at),
        kind: e.kind as ClusterEvent["kind"],
        nodeId: orNull(e.nodeId),
        text: e.text,
        ref: e.ref ? { type: e.ref.type as "command" | "workspace" | "node", id: e.ref.id } : null,
      }),
    ),
    totals24h: {
      savedSeconds: s.totals24h?.savedSeconds ?? 0,
      builds: s.totals24h?.builds ?? 0,
      forks: s.totals24h?.forks ?? 0,
      medianForkMillis: num(s.totals24h?.medianForkMillis),
      oomKills: s.totals24h?.oomKills ?? 0,
      parks: s.totals24h?.parks ?? 0,
    },
    series: Object.fromEntries(
      Object.entries(s.series).map(([node, v]) => [
        node,
        v.samples.map((p) => ({ t: num(p.t), cpuBusy: p.cpuBusy, memoryUsed: p.memoryUsed, memoryPressure: p.memoryPressure })),
      ]),
    ),
    load: s.load.map((l) => ({ t: num(l.t), running: l.running, queued: l.queued, parked: l.parked })),
    hourly: s.hourly.map((h) => ({
      hour: num(h.hour),
      builds: h.builds,
      spentSeconds: h.spentSeconds,
      savedSeconds: h.savedSeconds,
      forks: h.forks,
      parks: h.parks,
      queueWaitP95Seconds: h.queueWaitP95Seconds,
    })),
    nodeHourly: Object.fromEntries(
      Object.entries(s.nodeHourly).map(([node, v]) => [node, v.hours.map((h) => ({ hour: num(h.hour), cpuBusy: h.cpuBusy, memoryUsed: h.memoryUsed }))]),
    ),
    history: s.history.map(toFinished),
    sessions: s.sessions.map(
      (x): Session => ({
        id: x.id,
        agent: x.agent,
        startedAt: iso(x.startedAt),
        lastActiveAt: iso(x.lastActiveAt),
        endedAt: isoOrNull(x.endedAt),
        endReason: x.endReason,
        workspaces: x.workspaces.map((w) => ({
          id: w.id,
          repoId: w.repoId,
          nodeId: w.nodeId,
          createdAt: iso(w.createdAt),
          releasedAt: isoOrNull(w.releasedAt),
        })),
        schedule: orNull(x.schedule),
        scheduleRunning: x.scheduleRunning,
      }),
    ),
    schedules: s.schedules.map(toScheduled),
  };
}

function toScheduled(sc: PbScheduled): ScheduledCommand {
  return {
    name: sc.name,
    cron: sc.cron,
    repoId: sc.repo,
    node: orNull(sc.node),
    argv: shellJoin(sc.command?.args ?? []),
    env: sc.command?.env ?? [],
    paused: sc.paused,
    createdAt: iso(sc.createdAt),
    updatedAt: iso(sc.updatedAt),
    nextAt: isoOrNull(sc.nextAt),
    runs: sc.runs.map(toRun),
  };
}

const OUTCOME: Record<RunOutcome, ScheduledRun["outcome"]> = {
  [RunOutcome.UNSPECIFIED]: "running",
  [RunOutcome.RUNNING]: "running",
  [RunOutcome.SUCCEEDED]: "succeeded",
  [RunOutcome.FAILED]: "failed",
  [RunOutcome.CANCELLED]: "cancelled",
  [RunOutcome.NOT_STARTED]: "not-started",
  [RunOutcome.SKIPPED]: "skipped",
  [RunOutcome.MISSED]: "missed",
};

function toRun(r: PbRun): ScheduledRun {
  return {
    id: num(r.id),
    schedule: r.schedule,
    slotAt: isoOrNull(r.slotAt),
    startedAt: iso(r.startedAt),
    endedAt: isoOrNull(r.endedAt),
    outcome: OUTCOME[r.outcome],
    sessionId: orNull(r.sessionId),
    commandId: orNull(r.commandId),
    nodeId: orNull(r.nodeId),
    exitCode: r.exitCode ?? null,
    reason: r.reason,
  };
}

/** Words as a POSIX shell reads them back: plain ones bare, others single-quoted. */
function shellJoin(args: string[]): string {
  return args.map((a) => (/^[\w@%+=:,./-]+$/.test(a) ? a : `'${a.replaceAll("'", `'\\''`)}'`)).join(" ");
}

function toNode(n: PbNode): Node {
  const r = n.resources;
  const u = n.usage;
  const d = n.disk;
  return {
    id: n.id,
    hostname: n.hostname,
    arch: r?.arch === "amd64" ? "amd64" : "arm64",
    machine: r?.machine ?? "",
    kernel: r?.kernel ?? "",
    online: n.online,
    joinedAt: iso(n.joinedAt),
    lastSeen: iso(n.lastSeen ?? n.joinedAt),
    cpus: r?.cpus ?? 0,
    memoryBytes: num(r?.memoryBytes),
    swapBytes: num(r?.swapBytes),
    diskBytes: num(n.diskBytes),
    usage: {
      cpuBusy: u?.cpuBusy ?? 0,
      memoryUsedBytes: num(u?.memoryUsedBytes),
      swapUsedBytes: num(u?.swapUsedBytes),
      cpuPressure: u?.cpuPressure ?? 0,
      memoryPressure: u?.memoryPressure ?? 0,
    },
    reservedMemoryBytes: num(n.reservedMemoryBytes),
    disk: {
      basesBytes: num(d?.basesBytes),
      forksExclusiveBytes: num(d?.forksExclusiveBytes),
      otherBytes: num(d?.otherBytes),
      freeBytes: num(d?.freeBytes),
    },
  };
}

function toBase(b: PbBase): Base {
  return {
    id: b.id,
    repoId: b.repoId,
    nodeId: b.nodeId,
    generation: b.generation,
    commit: b.commit,
    message: b.message,
    // A base that failed to freshen is as unusable as a stale one.
    state: b.state === BaseState.BUILDING ? "building" : b.state === BaseState.WARM ? "warm" : "stale",
    progress: b.progress,
    builtAt: isoOrNull(b.builtAt),
    buildSeconds: b.buildSeconds,
    sizeBytes: num(b.sizeBytes),
    toolchain: b.toolchain,
    baked: b.baked,
  };
}

function toWorkspace(w: PbWorkspace): Workspace {
  return {
    id: w.id,
    sessionId: w.sessionId,
    repoId: w.repoId,
    baseId: w.baseId,
    nodeId: w.nodeId,
    agent: w.agent,
    branch: w.branch || w.sessionId,
    head: w.head.slice(0, 10),
    filesChanged: w.filesChanged,
    state: workspaceState(w.state),
    reason: w.reason,
    createdAt: iso(w.createdAt),
    forkMillis: num(w.forkMillis),
    exclusiveBytes: num(w.exclusiveBytes),
    sharedBytes: num(w.sharedBytes),
    lastActiveAt: iso(w.lastActiveAt),
  };
}

function workspaceState(s: WorkspaceState): Workspace["state"] {
  switch (s) {
    case WorkspaceState.PREPARING:
      return "preparing";
    case WorkspaceState.ACTIVE:
      return "active";
    case WorkspaceState.RELEASED:
      return "released";
    case WorkspaceState.FAILED:
      return "failed";
    default:
      return "idle";
  }
}

function state(s: CommandState): Command["state"] {
  switch (s) {
    case CommandState.QUEUED:
      return "queued";
    case CommandState.RUNNING:
      return "running";
    case CommandState.PARKED:
      return "parked";
    case CommandState.SUCCEEDED:
      return "succeeded";
    case CommandState.FAILED:
      return "failed";
    default:
      return "cancelled";
  }
}

function kind(k: string): Command["kind"] {
  return k === "build" || k === "test" || k === "check" || k === "server" ? k : "shell";
}

function toCommand(c: PbCommand): Command {
  return {
    id: c.id,
    sessionId: c.sessionId,
    workspaceId: c.workspaceId,
    nodeId: c.nodeId,
    argv: c.argv,
    kind: kind(c.kind),
    state: state(c.state),
    queuedAt: iso(c.queuedAt),
    startedAt: isoOrNull(c.startedAt),
    endedAt: isoOrNull(c.endedAt),
    exitCode: c.exitCode ?? null,
    limits: {
      cpuWeight: c.limits?.cpuWeight ?? 100,
      memoryLowBytes: num(c.limits?.memoryLowBytes),
      memoryHighBytes: num(c.limits?.memoryHighBytes),
    },
    cpuCores: c.cpuCores,
    memoryBytes: num(c.memoryBytes),
    swapBytes: num(c.swapBytes),
    rebuilt: c.rebuilt ? { units: c.rebuilt.units, of: c.rebuilt.of } : null,
    coldSeconds: c.coldSeconds ?? null,
    reason: orNull(c.reason),
    url: orNull(c.url),
    timeline: c.timeline.map((e) => ({ at: iso(e.at), what: e.what as Command["timeline"][number]["what"], detail: e.detail })),
  };
}

function toFinished(f: PbFinished): FinishedCommand {
  const st = state(f.state);
  return {
    id: f.id,
    sessionId: f.sessionId,
    repoId: f.repoId,
    nodeId: f.nodeId,
    workspaceId: f.workspaceId,
    agent: f.agent,
    argv: f.argv,
    kind: kind(f.kind),
    state: st === "succeeded" || st === "failed" ? st : "cancelled",
    exitCode: f.exitCode ?? null,
    failure: (orNull(f.failure) as FailureKind | null) ?? null,
    endedAt: num(f.endedAt),
    seconds: f.seconds,
    coldSeconds: f.coldSeconds ?? null,
    rebuilt: f.rebuilt ?? null,
    of: f.of ?? null,
    peakMemoryBytes: num(f.peakMemoryBytes),
    parks: f.parks,
  };
}
