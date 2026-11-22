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
  /** Its canonical git remote, which projects hold it by. */
  remote: string;
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
  /** The project it works on; null for one from before projects. */
  project: string | null;
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
  /** The project its session works on. */
  project?: string | null;
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
  /** The project its session works on. */
  project?: string | null;
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
  /** The project its session worked on; null for a sessionless freshen. */
  project?: string | null;
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
  /** Questions waiting for the user's answer, every project's. */
  openQuestions: number;
  /** Every project, by name. */
  projects: Project[];
}

/**
 * What sessions work on and questions are about: one or more repos, each
 * named by its canonical git remote, as a T3 Code project holds folders.
 */
export interface Project {
  name: string;
  /** Canonical git remotes: github.com/semistrict/hotbed. */
  repos: string[];
  createdAt: string;
  liveSessions: number;
  openQuestions: number;
  /** Questions answered or dismissed, waiting for an agent. */
  outbox: number;
  /** Reviews requested or in progress. */
  activeReviews: number;
  /** Reviews done with findings no one has decided yet. */
  reviewsToDecide: number;
  /** Scheduled commands in this project. */
  schedules: number;
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
  /** The project its runs work on; null for one added before schedules had one. */
  project: string | null;
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

/**
 * open: waiting for the user. answered and dismissed: in the outbox, until
 * an agent acknowledges the response, which makes it handled. deleted: a
 * session took it off the list. Nothing is ever removed.
 */
export type QuestionState = "open" | "answered" | "dismissed" | "handled" | "deleted";

/** newest or oldest asked first, the most recently changed first, or the user's own order. */
export type QuestionOrder = "newest" | "oldest" | "updated" | "rank";

/** What an agent asks, as AskUserQuestion takes it. */
export interface Prompt {
  question: string;
  /** At most 12 characters. */
  header: string;
  /** Two to four. */
  options: PromptOption[];
  multiSelect: boolean;
}

export interface PromptOption {
  label: string;
  description: string;
  /** Shown beside the option when it is focused: a mockup, code; empty for none. */
  preview: string;
}

/** Who changed a question and when: an agent's session, or the user (null). */
export interface Stamp {
  at: string;
  session: string | null;
  host: string;
  /** A dismissal's or a deletion's reason. */
  reason: string;
}

export interface Answer {
  /** The labels picked: one at most unless multiSelect. */
  picked: string[];
  /** Words of the user's own, instead of picking or besides. */
  other: string;
  notes: string;
}

/** The asking agent's checkout, when it was in one. */
export interface AskedFrom {
  dir: string;
  remote: string;
  branch: string;
  head: string;
  dirtyFiles: number;
  /** The first lines of git status --porcelain. */
  dirty: string[];
}

export interface Question {
  id: number;
  project: string;
  /** The project's repo it is about, a canonical remote; null for the whole project. */
  repo: string | null;
  prompt: Prompt;
  state: QuestionState;
  /** Its place in the user's order, lower first. */
  rank: number;
  answer: Answer | null;
  asked: Stamp;
  askedFrom: AskedFrom | null;
  edited: Stamp | null;
  answered: Stamp | null;
  dismissed: Stamp | null;
  handled: Stamp | null;
  deleted: Stamp | null;
  /** Its last change other than a move. */
  updatedAt: string;
}

/** One change to a question, as its history keeps it. */
export interface QuestionChange {
  seq: number;
  /** asked, edited, moved, answered, dismissed, handled, deleted */
  what: string;
  by: Stamp;
  /** The question as the change left it. */
  question: Question;
}

export interface QuestionFilter {
  /** Only this project's; null or unset for every project's. */
  project?: string | null;
  /** None: every state but deleted. */
  states?: QuestionState[];
  order?: QuestionOrder;
}

export interface QuestionList {
  questions: Question[];
  total: number;
}

/** requested: waiting for a reviewer; in-progress: claimed, finding; then one of three ends. */
export type ReviewState = "requested" | "in-progress" | "done" | "failed" | "cancelled";

/** As ReportFindings grades it: a bug the change adds, one that was there before, a minor issue. */
export type Severity = "important" | "pre-existing" | "nit";

/** What the requesting agent made of a finding. */
export type Decision = "open" | "accepted" | "dismissed";

/** What the user can say of a finding with one click. */
export type Quick = "verify" | "fix-if-verified" | "wont-fix";

/** A place in the reviewed version. */
export interface Location {
  path: string;
  /** 1-based; 0 for the whole file. */
  startLine: number;
  endLine: number;
  /** The lines as the reviewer saw them; empty when it gave none. */
  excerpt: string;
}

export interface Finding {
  id: number;
  reviewId: number;
  title: string;
  summary: string;
  failureScenario: string;
  category: string;
  severity: Severity;
  /** How sure the reviewer is; null when not verified. */
  verdict: "confirmed" | "plausible" | null;
  /** Zero or more. */
  locations: Location[];
  decision: Decision;
  decided: Stamp | null;
  found: Stamp;
  /** The latest quick response to it. */
  quick: Quick | null;
  responses: number;
}

/** Something that happened to a review: a change, or a response (what "responded"). */
export interface ReviewEvent {
  seq: number;
  reviewId: number;
  /** null for the review as a whole. */
  findingId: number | null;
  what: string;
  by: Stamp;
  quick: Quick | null;
  text: string;
}

export interface Review {
  id: number;
  project: string;
  /** Canonical remote; null outside a repo. */
  repo: string | null;
  title: string;
  instructions: string;
  /** null: the whole tree at head. */
  base: string | null;
  head: string;
  branch: string;
  /** head is a snapshot of uncommitted changes on this commit. */
  snapshotOf: string | null;
  state: ReviewState;
  /** The reviewer's summary when done; why it failed or was cancelled. */
  summary: string;
  requested: Stamp;
  started: Stamp | null;
  ended: Stamp | null;
  updatedAt: string;
  /** Only from review(id). */
  findings: Finding[];
  events: ReviewEvent[];
  counts: { important: number; preExisting: number; nits: number; open: number; responses: number };
}

export interface Api {
  snapshot(): Promise<Snapshot>;
  commandLog(id: string): Promise<CommandLog>;
  files(workspaceId: string): Promise<FileEntry[]>;
  file(workspaceId: string, path: string): Promise<FileContent>;
  questions(filter: QuestionFilter): Promise<QuestionList>;
  /** Every change to a question, oldest first. */
  questionHistory(id: number): Promise<QuestionChange[]>;
  answerQuestion(id: number, answer: Answer): Promise<Question>;
  dismissQuestion(id: number, reason: string): Promise<Question>;
  /** Puts an open question just before another, or last (before null). */
  moveQuestion(id: number, before: number | null): Promise<Question>;
  /** A project's reviews (every project's for null), newest first, without findings and events. */
  reviews(project: string | null): Promise<Review[]>;
  review(id: number): Promise<Review>;
  /** The user's response to a finding (or, findingId null, the review): a quick response, a comment, or both. */
  respond(reviewId: number, findingId: number | null, quick: Quick | null, text: string): Promise<ReviewEvent>;
}
