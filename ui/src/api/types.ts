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
  /** The CPUs a measurement has to itself (a cpulist); "" on a node that takes none. */
  benchCpus: string;
  /** Status of the always-on OpenTelemetry eBPF continuous profiler on this worker. */
  profiler: ProfilerStatus | null;
}

export interface ProfilerStatus {
  active: boolean;
  engine: string;
  sampleHz: number;
  totalSamples: number;
  error: string;
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

/**
 * An agent's session: everything it runs belongs to one. It holds a workspace
 * per repo it works in, and ends when its agent ends it or after an hour
 * with nothing touching it (abandoned).
 */
export interface Session {
  /** adjective-noun, such as brave-otter. */
  id: string;
  /** The machine the agent runs on. */
  agent: string;
  startedAt: string;
  lastActiveAt: string;
  endedAt: string | null;
  /** Why it ended: by its agent, abandoned, or its scheduled run's end. */
  endReason: string;
  /** Every workspace it had, released ones too, oldest first. */
  workspaces: SessionWorkspace[];
  /** The scheduled command this session is a run of; null for an agent's. */
  schedule: string | null;
  /** Its scheduled run is in progress: the idle hour does not apply. */
  scheduleRunning: boolean;
}

export interface SessionWorkspace {
  id: string;
  repoId: string;
  nodeId: string;
  createdAt: string;
  releasedAt: string | null;
}

/** A fork of a base that one agent works in. */
export interface Workspace {
  id: string;
  sessionId: string;
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
  sessionId: string;
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
  /** A measurement (hotbed run --bench): alone on the node's measurement CPUs, isolated, never parked. */
  bench: boolean;
  timeline: CommandEvent[];
}

export interface CommandEvent {
  at: string;
  what: "queued" | "admitted" | "parked" | "thawed" | "isolation lost" | "exited" | "cancelled";
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
  sessionId: string;
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
  bench: boolean;
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
  /** Live sessions and those that ended in the last 24 hours, the most recently active first. */
  sessions: Session[];
  /** By name, each with its recent runs. */
  schedules: ScheduledCommand[];
}

/**
 * Recurring work the controller starts by itself. Each run is a session of
 * its own, with a fresh workspace of the repo, in which the command runs.
 */
export interface ScheduledCommand {
  name: string;
  /** Five-field cron, in the controller's time zone unless CRON_TZ= says otherwise. */
  cron: string;
  repoId: string;
  /** The node every run is pinned to; null lets the controller place each. */
  node: string | null;
  /** The command, shell-quoted, as it runs at /work/REPO. */
  argv: string;
  env: string[];
  paused: boolean;
  createdAt: string;
  updatedAt: string;
  /** When the next run is due; null while paused. */
  nextAt: string | null;
  /** The newest first. */
  runs: ScheduledRun[];
}

/**
 * running and the three ends of a command; not-started: it could not be
 * placed; skipped: the previous run was still in progress at its slot;
 * missed: its slot passed while the controller was down.
 */
export type RunOutcome = "running" | "succeeded" | "failed" | "cancelled" | "not-started" | "skipped" | "missed";

export interface ScheduledRun {
  id: number;
  schedule: string;
  /** The slot it was due at; null for a run started by hand. */
  slotAt: string | null;
  startedAt: string;
  endedAt: string | null;
  outcome: RunOutcome;
  /** Null for a slot skipped or missed, which started nothing. */
  sessionId: string | null;
  commandId: string | null;
  nodeId: string | null;
  exitCode: number | null;
  /** Why it failed, was skipped or missed, or did not start. */
  reason: string;
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

export type InspectSection = "procs" | "files" | "cgroup" | "kstack" | "maps" | "limits";

export interface ThreadInfo {
  tid: number;
  name: string;
  state: string;
  wchan: string;
  cpuUserMs: number;
  cpuSysMs: number;
}

export interface ProcessInfo {
  pid: number;
  ppid: number;
  state: string;
  comm: string;
  cmdline: string;
  threadsCount: number;
  rssBytes: number;
  vsizeBytes: number;
  cpuUserMs: number;
  cpuSysMs: number;
  ioReadBytes: number;
  ioWriteBytes: number;
  wchan: string;
  oomScore: number;
  voluntaryCtxtSwitches: number;
  nonvoluntaryCtxtSwitches: number;
  threads: ThreadInfo[];
}

export interface OpenFile {
  pid: number;
  comm: string;
  fd: number;
  kind: "file" | "socket" | "pipe" | "anon_inode" | "device" | "deleted" | "other";
  target: string;
  mode: string;
  pos: number;
  inode: number;
  socketProto: string;
  socketState: string;
  localAddr: string;
  remoteAddr: string;
}

export interface CgroupIO {
  device: string;
  rbytes: number;
  wbytes: number;
  rios: number;
  wios: number;
}

export interface CgroupStats {
  cpuUsageUsec: number;
  cpuUserUsec: number;
  cpuSystemUsec: number;
  nrThrottled: number;
  throttledUsec: number;
  memoryCurrentBytes: number;
  memoryPeakBytes: number;
  memoryAnonBytes: number;
  memoryFileBytes: number;
  memoryKernelBytes: number;
  memorySlabBytes: number;
  memorySockBytes: number;
  memoryShmemBytes: number;
  memorySwapBytes: number;
  pgfault: number;
  pgmajfault: number;
  pidsCurrent: number;
  pidsPeak: number;
  psiCpuSomeAvg10: number;
  psiCpuFullAvg10: number;
  psiMemSomeAvg10: number;
  psiMemFullAvg10: number;
  psiIoSomeAvg10: number;
  psiIoFullAvg10: number;
  io: CgroupIO[];
}

export interface ThreadKernelStack {
  pid: number;
  tid: number;
  comm: string;
  threadName: string;
  state: string;
  wchan: string;
  frames: string[];
}

export interface MemoryRegion {
  perms: string;
  path: string;
  sizeBytes: number;
  rssBytes: number;
  pssBytes: number;
}

export interface ProcessMemoryMap {
  pid: number;
  comm: string;
  rssBytes: number;
  pssBytes: number;
  privateDirtyBytes: number;
  sharedCleanBytes: number;
  swapBytes: number;
  anonBytes: number;
  shmemBytes: number;
  topRegions: MemoryRegion[];
}

export interface ResourceLimit {
  name: string;
  soft: string;
  hard: string;
  units: string;
}

export interface ProcessLimits {
  pid: number;
  comm: string;
  items: ResourceLimit[];
  capEff: string;
  seccomp: string;
  noNewPrivs: boolean;
}

export interface ToolCapability {
  name: string;
  available: boolean;
  detail: string;
}

export interface InspectCommandResult {
  commandId: string;
  collectedAt: number;
  cgroup: string;
  unitName: string;
  procs: ProcessInfo[];
  files: OpenFile[];
  cgroupStats: CgroupStats | null;
  kstacks: ThreadKernelStack[];
  maps: ProcessMemoryMap[];
  limits: ProcessLimits[];
  availableTools: ToolCapability[];
}

export type TraceKind =
  | "strace"
  | "strace_summary"
  | "perf_cpu"
  | "perf_stat"
  | "stacks"
  | "bpf_execsnoop"
  | "bpf_opensnoop"
  | "bpf_vfsstat"
  | "bpf_offcpu"
  | "bpf_net";

export interface TraceOptions {
  kind: TraceKind;
  durationSeconds?: number;
  pid?: number;
  filter?: string;
  frequencyHz?: number;
}

export interface PerfCounter {
  name: string;
  value: string;
  unit: string;
  note: string;
}

export interface SyscallStat {
  name: string;
  pctTime: number;
  seconds: number;
  usecsPerCall: number;
  calls: number;
  errors: number;
}

export interface FoldedStack {
  stack: string;
  samples: number;
  leafType: string;
  comm: string;
}

export interface ProfileFunction {
  name: string;
  module: string;
  frameType: string;
  sourceFile: string;
  sourceLine: number;
  selfSamples: number;
  totalSamples: number;
  selfPct: number;
  totalPct: number;
}

export interface ProfileBucket {
  t: number;
  samples: number;
  kernelSamples: number;
  userSamples: number;
  topFunction: string;
}

export interface TraceSummary {
  kind: TraceKind;
  startedAt: number;
  endedAt: number;
  durationMs: number;
  pidsAttached: number[];
  commandLine: string;
  stacks: FoldedStack[];
  topFunctions: ProfileFunction[];
  counters: PerfCounter[];
  syscalls: SyscallStat[];
  raw: string;
}

export interface TraceChunk {
  status: string;
  text: string;
  summary: TraceSummary | null;
  done: boolean;
}

export interface ProfileQuery {
  query?: string;
  frameType?: string;
  comm?: string;
  fromMs?: number;
  toMs?: number;
  limit?: number;
}

export interface CommandProfile {
  commandId: string;
  profiler: string;
  live: boolean;
  active: boolean;
  statusNote: string;
  sampleHz: number;
  startedAt: number;
  updatedAt: number;
  totalSamples: number;
  matchedSamples: number;
  kernelSamples: number;
  userSamples: number;
  comms: string[];
  frameTypes: string[];
  stacks: FoldedStack[];
  topFunctions: ProfileFunction[];
  buckets: ProfileBucket[];
}

export interface Api {
  snapshot(): Promise<Snapshot>;
  commandLog(id: string): Promise<CommandLog>;
  files(workspaceId: string): Promise<FileEntry[]>;
  file(workspaceId: string, path: string): Promise<FileContent>;
  inspectCommand(id: string, opts?: { sections?: InspectSection[]; pid?: number }): Promise<InspectCommandResult>;
  traceCommand(id: string, opts: TraceOptions, onChunk: (chunk: TraceChunk) => void, signal?: AbortSignal): Promise<TraceSummary | null>;
  commandProfile(id: string, query?: ProfileQuery): Promise<CommandProfile>;
}
