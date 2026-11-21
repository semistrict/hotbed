// The hotbed API as the UI sees it. The mock in ./mock implements it today;
// the controller's /v1 endpoints will serve the same shapes.

export type Arch = "arm64" | "amd64";

/** A share of a whole, 0..1. */
export type Fraction = number;

export interface NodeUsage {
  /** CPU busy across all of the node's CPUs since the last sample. */
  cpuBusy: Fraction;
  memoryUsedBytes: number;
  swapUsedBytes: number;
  /** PSI "some" avg10, percent of time at least one task stalled. */
  cpuPressure: number;
  memoryPressure: number;
}

export interface Node {
  id: string;
  hostname: string;
  arch: Arch;
  /** What the machine is, as a person would say it: "MacBook Pro (M3 Pro)". */
  machine: string;
  kernel: string;
  online: boolean;
  joinedAt: string;
  lastSeen: string;
  cpus: number;
  memoryBytes: number;
  swapBytes: number;
  diskBytes: number;
  usage: NodeUsage;
  /** What admitted commands are guaranteed: the sum of their memory.low. */
  reservedMemoryBytes: number;
  disk: DiskUsage;
}

/** A node's btrfs pool, split by what the blocks belong to. */
export interface DiskUsage {
  /** Warm bases: the built checkouts every fork shares. */
  basesBytes: number;
  /** Blocks only one fork owns: what its builds rewrote. */
  forksExclusiveBytes: number;
  otherBytes: number;
  freeBytes: number;
}

export interface Repo {
  id: string;
  name: string;
  language: "rust" | "go" | "typescript";
  /** Crates (or packages) in the workspace. */
  units: number;
  /** A full build from nothing on a typical node. */
  coldBuildSeconds: number;
}

export type BaseState = "building" | "warm" | "stale";

/**
 * A warm base: one repo's main, checked out and built on one node, kept as a
 * read-only btrfs snapshot that workspaces fork from.
 */
export interface Base {
  id: string;
  repoId: string;
  nodeId: string;
  generation: number;
  commit: string;
  message: string;
  state: BaseState;
  /** 0..1 while building. */
  progress: Fraction;
  builtAt: string | null;
  buildSeconds: number;
  sizeBytes: number;
  toolchain: string;
  /** The commands built into the base, so forks start with their artifacts. */
  baked: string[];
}

/** Preparing: waiting for its base to be pulled and freshened. */
export type WorkspaceState = "preparing" | "active" | "idle" | "released" | "failed";

/** A fork of a base that one agent works in. */
export interface Workspace {
  id: string;
  repoId: string;
  baseId: string;
  nodeId: string;
  agent: string;
  branch: string;
  head: string;
  filesChanged: number;
  state: WorkspaceState;
  /** What a preparing workspace waits for, or why a failed one failed. */
  reason: string;
  createdAt: string;
  forkMillis: number;
  exclusiveBytes: number;
  sharedBytes: number;
  lastActiveAt: string;
}

export type CommandKind = "build" | "test" | "check" | "server" | "shell";
export type CommandState = "queued" | "running" | "parked" | "succeeded" | "failed" | "cancelled";

export interface CommandLimits {
  cpuWeight: number;
  memoryLowBytes: number;
  memoryHighBytes: number;
}

export interface Command {
  id: string;
  workspaceId: string;
  nodeId: string;
  argv: string;
  kind: CommandKind;
  state: CommandState;
  queuedAt: string;
  startedAt: string | null;
  endedAt: string | null;
  exitCode: number | null;
  limits: CommandLimits;
  /** CPU in cores and memory in bytes, now (or at the end). */
  cpuCores: number;
  memoryBytes: number;
  swapBytes: number;
  /** Cargo's units that compiled; null for commands that build nothing. */
  rebuilt: { units: number; of: number } | null;
  /** How long the same command takes from nothing; null when it builds nothing. */
  coldSeconds: number | null;
  /** Why it waits (queued) or sleeps (parked). */
  reason: string | null;
  /** A dev server's address. */
  url: string | null;
  timeline: CommandEvent[];
}

export interface CommandEvent {
  at: string;
  what: "queued" | "admitted" | "parked" | "thawed" | "exited" | "cancelled";
  detail: string;
}

export type EventKind = "fork" | "release" | "admit" | "finish" | "fail" | "park" | "thaw" | "base" | "node";

export interface ClusterEvent {
  id: number;
  at: string;
  kind: EventKind;
  nodeId: string | null;
  text: string;
  /** The command, workspace or base it is about, for a link. */
  ref: { type: "command" | "workspace" | "node"; id: string } | null;
}

export interface Totals {
  /** Cold build time minus actual, summed over builds that finished in the window. */
  savedSeconds: number;
  builds: number;
  forks: number;
  medianForkMillis: number;
  oomKills: number;
  parks: number;
}

export interface Sample {
  t: number; // unix ms
  cpuBusy: Fraction;
  memoryUsed: Fraction;
  memoryPressure: number;
}

/** The cluster's command counts at one sample. */
export interface LoadSample {
  t: number;
  running: number;
  queued: number;
  parked: number;
}

/** One hour of the cluster's history, as the controller keeps it in SQLite. */
export interface HourBucket {
  hour: number; // unix ms at the start of the hour
  builds: number;
  /** Compile time spent and saved by builds that finished in the hour. */
  spentSeconds: number;
  savedSeconds: number;
  forks: number;
  parks: number;
  queueWaitP95Seconds: number;
}

export interface NodeHour {
  hour: number;
  cpuBusy: Fraction;
  memoryUsed: Fraction;
}

/**
 * A command that ended in the last 24 hours, as the controller keeps it in
 * SQLite: the history, the failures, and the distributions come from these.
 */
export interface FinishedCommand {
  id: string;
  repoId: string;
  nodeId: string;
  workspaceId: string;
  agent: string;
  argv: string;
  kind: CommandKind;
  state: "succeeded" | "failed" | "cancelled";
  exitCode: number | null;
  /** Why it failed, sorted into a few kinds; null when it succeeded. */
  failure: FailureKind | null;
  endedAt: number;
  seconds: number;
  /** Builds and tests only. */
  coldSeconds: number | null;
  rebuilt: number | null;
  of: number | null;
  peakMemoryBytes: number;
  /** Times it was frozen for memory and thawed again. */
  parks: number;
}

export type FailureKind = "compile" | "test" | "lint" | "signal" | "exit";

export interface Snapshot {
  clusterId: string;
  controller: string;
  now: string;
  /** Seconds between live samples. */
  sampleSeconds: number;
  nodes: Node[];
  repos: Repo[];
  bases: Base[];
  workspaces: Workspace[];
  commands: Command[];
  events: ClusterEvent[];
  totals24h: Totals;
  /** Live samples per node ID, oldest first. */
  series: Record<string, Sample[]>;
  load: LoadSample[];
  /** The last 24 hours, oldest first. */
  hourly: HourBucket[];
  nodeHourly: Record<string, NodeHour[]>;
  history: FinishedCommand[];
}

export interface CommandLog {
  commandId: string;
  lines: string[];
}

export interface FileEntry {
  path: string;
  kind: "file" | "dir";
  sizeBytes: number;
  /** Against the workspace's base: added, modified, or unchanged. */
  change: "added" | "modified" | null;
}

export interface FileContent {
  path: string;
  text: string;
  /** Line numbers (1-based) the workspace changed against its base. */
  changedLines: number[];
}

export interface Api {
  snapshot(): Promise<Snapshot>;
  commandLog(id: string): Promise<CommandLog>;
  files(workspaceId: string): Promise<FileEntry[]>;
  file(workspaceId: string, path: string): Promise<FileContent>;
}
