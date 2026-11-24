// Hotbed API types: generated protobuf-es messages and enums from hotbed.proto,
// plus small client request option types and enum display helpers.

import type { MessageInitShape } from "@bufbuild/protobuf";
import {
  BaseState,
  CommandState,
  DaemonLogLevel,
  FindingDecision,
  FindingSeverity,
  FindingVerdict,
  InspectSection,
  InterventionKind,
  MemoryMode,
  MemoryOrder,
  MemoryScope,
  MemoryState,
  QuestionOrder,
  QuestionState,
  QuickResponse,
  ReviewState,
  RunOutcome,
  StepState,
  TraceKind,
  WorkspaceState,
  type Admission,
  type AdmissionSignal,
  type Answer,
  type AnswerSchema,
  type AskedFrom,
  type Base,
  type BaseRepo,
  type CgroupIO,
  type CgroupStats,
  type ClusterEvent,
  type Command,
  type CommandDiagnostics,
  type CommandEvent,
  type CommandIntervention,
  type CommandLimits,
  type CommandLog,
  type CommandProfile,
  type CostPart,
  type DaemonLogEntry,
  type DaemonLogSource,
  type DiskUsage,
  type EventRef,
  type FileContent,
  type FileEntry,
  type FileLock,
  type Finding,
  type FinishedCommand,
  type Fix,
  type FoldedStack,
  type GetDaemonLogsResponse,
  type HostUsage,
  type HourBucket,
  type InspectCommandResponse,
  type ListMemoriesResponse,
  type ListQuestionsResponse,
  type LoadSample,
  type Location,
  type Memory,
  type MemoryBriefing,
  type MemoryChange,
  type MemoryContent,
  type MemoryContentSchema,
  type MemoryIndexEntry,
  type MemoryProvenance,
  type MemoryRegion,
  type Node,
  type NodeHour,
  type NodeHours,
  type NodeUsage,
  type ObjectStoreStats,
  type OpenFile,
  type PerfCounter,
  type PlacementCandidate,
  type PlacementDecision,
  type ProcessInfo,
  type ProcessLimits,
  type ProcessMemoryMap,
  type ProcessSample,
  type ProfileBucket,
  type ProfileFrame,
  type ProfileFunction,
  type ProfilerStatus,
  type Project,
  type Prompt,
  type PromptOption,
  type PromptSchema,
  type Question,
  type QuestionChange,
  type Rebuilt,
  type RepoSummary,
  type ResourceLimit,
  type Resources,
  type Review,
  type ReviewEvent,
  type RunCommandRequest,
  type Sample,
  type SampleCollector,
  type SampleCommandChunk,
  type SampledCgroup,
  type SampledOpenFile,
  type SampledProcess,
  type SampledThread,
  type Samples,
  type ScheduledCommand,
  type ScheduledRun,
  type Session,
  type SessionWorkspace,
  type Snapshot,
  type StackFrame,
  type Stamp,
  type Step,
  type StepOnNode,
  type StepStatus,
  type SyscallStat,
  type ThreadInfo,
  type ThreadKernelStack,
  type ThreadSyscall,
  type ToolCapability,
  type Totals,
  type TraceCommandChunk,
  type TraceSummary,
  type Workspace,
  type WorkspaceRepo,
} from "../gen/hotbed/v1/hotbed_pb";

export {
  BaseState,
  CommandState,
  DaemonLogLevel,
  FindingDecision,
  FindingSeverity,
  FindingVerdict,
  InspectSection,
  InterventionKind,
  MemoryMode,
  MemoryOrder,
  MemoryScope,
  MemoryState,
  QuestionOrder,
  QuestionState,
  QuickResponse,
  ReviewState,
  RunOutcome,
  StepState,
  TraceKind,
  WorkspaceState,
  type Admission,
  type AdmissionSignal,
  type Answer,
  type AskedFrom,
  type Base,
  type BaseRepo,
  type CgroupIO,
  type CgroupStats,
  type ClusterEvent,
  type Command,
  type CommandDiagnostics,
  type CommandEvent,
  type CommandIntervention,
  type CommandLimits,
  type CommandLog,
  type CommandProfile,
  type CostPart,
  type DaemonLogEntry,
  type DaemonLogSource,
  type DiskUsage,
  type EventRef,
  type FileContent,
  type FileEntry,
  type FileLock,
  type Finding,
  type FinishedCommand,
  type Fix,
  type FoldedStack,
  type GetDaemonLogsResponse,
  type HostUsage,
  type HourBucket,
  type InspectCommandResponse,
  type ListMemoriesResponse,
  type ListQuestionsResponse,
  type LoadSample,
  type Location,
  type Memory,
  type MemoryBriefing,
  type MemoryChange,
  type MemoryContent,
  type MemoryContentSchema,
  type MemoryIndexEntry,
  type MemoryProvenance,
  type MemoryRegion,
  type Node,
  type NodeHour,
  type NodeHours,
  type NodeUsage,
  type ObjectStoreStats,
  type OpenFile,
  type PerfCounter,
  type PlacementCandidate,
  type PlacementDecision,
  type ProcessInfo,
  type ProcessLimits,
  type ProcessMemoryMap,
  type ProcessSample,
  type ProfileBucket,
  type ProfileFrame,
  type ProfileFunction,
  type ProfilerStatus,
  type Project,
  type Prompt,
  type PromptOption,
  type Question,
  type QuestionChange,
  type Rebuilt,
  type RepoSummary,
  type ResourceLimit,
  type Resources,
  type Review,
  type ReviewEvent,
  type RunCommandRequest,
  type Sample,
  type SampleCollector,
  type SampleCommandChunk,
  type SampledCgroup,
  type SampledOpenFile,
  type SampledProcess,
  type SampledThread,
  type Samples,
  type ScheduledCommand,
  type ScheduledRun,
  type Session,
  type SessionWorkspace,
  type Snapshot,
  type StackFrame,
  type Stamp,
  type Step,
  type StepOnNode,
  type StepStatus,
  type SyscallStat,
  type ThreadInfo,
  type ThreadKernelStack,
  type ThreadSyscall,
  type ToolCapability,
  type Totals,
  type TraceCommandChunk,
  type TraceSummary,
  type Workspace,
  type WorkspaceRepo,
};

export type AnswerInput = MessageInitShape<typeof AnswerSchema>;
export type PromptInput = MessageInitShape<typeof PromptSchema>;
export type MemoryContentInput = MessageInitShape<typeof MemoryContentSchema>;

export type CommandStateSlug = "queued" | "running" | "parked" | "paused" | "succeeded" | "failed" | "cancelled";
export const COMMAND_STATE_FROM_SLUG: Record<CommandStateSlug, CommandState> = {
  queued: CommandState.QUEUED,
  running: CommandState.RUNNING,
  parked: CommandState.PARKED,
  paused: CommandState.PAUSED,
  succeeded: CommandState.SUCCEEDED,
  failed: CommandState.FAILED,
  cancelled: CommandState.CANCELLED,
};

export type QuestionOrderSlug = "newest" | "oldest" | "updated" | "rank";
export const QUESTION_ORDER_FROM_SLUG: Record<QuestionOrderSlug, QuestionOrder> = {
  newest: QuestionOrder.NEWEST,
  oldest: QuestionOrder.OLDEST,
  updated: QuestionOrder.UPDATED,
  rank: QuestionOrder.RANK,
};

export type MemoryOrderSlug = "rank" | "newest" | "oldest" | "updated" | "slug";
export const MEMORY_ORDER_FROM_SLUG: Record<MemoryOrderSlug, MemoryOrder> = {
  rank: MemoryOrder.RANK,
  newest: MemoryOrder.NEWEST,
  oldest: MemoryOrder.OLDEST,
  updated: MemoryOrder.UPDATED,
  slug: MemoryOrder.SLUG,
};

export type DaemonLogLevelSlug = "warn" | "error";
export const DAEMON_LOG_LEVEL_FROM_SLUG: Record<DaemonLogLevelSlug, DaemonLogLevel> = {
  warn: DaemonLogLevel.WARN,
  error: DaemonLogLevel.ERROR,
};

/** Offline nodes' last heartbeat admission no longer holds. */
export function nodeAdmission(n: Node): Admission | undefined {
  return n.online ? n.admission : undefined;
}

export function baseStateLabel(s: BaseState): string {
  switch (s) {
    case BaseState.BUILDING:
      return "building";
    case BaseState.WARM:
      return "warm";
    case BaseState.STALE:
      return "stale";
    case BaseState.FAILED:
      return "failed";
    default:
      return "unknown";
  }
}

export function workspaceStateLabel(s: WorkspaceState): string {
  switch (s) {
    case WorkspaceState.PREPARING:
      return "preparing";
    case WorkspaceState.ACTIVE:
      return "active";
    case WorkspaceState.IDLE:
      return "idle";
    case WorkspaceState.RELEASED:
      return "released";
    case WorkspaceState.FAILED:
      return "failed";
    default:
      return "unknown";
  }
}

export function stepStateLabel(s: StepState): string {
  switch (s) {
    case StepState.APPLIED:
      return "applied";
    case StepState.FAILED:
      return "failed";
    default:
      return "pending";
  }
}

export function runOutcomeLabel(o: RunOutcome): string {
  switch (o) {
    case RunOutcome.RUNNING:
      return "running";
    case RunOutcome.SUCCEEDED:
      return "succeeded";
    case RunOutcome.FAILED:
      return "failed";
    case RunOutcome.CANCELLED:
      return "cancelled";
    case RunOutcome.NOT_STARTED:
      return "not-started";
    case RunOutcome.SKIPPED:
      return "skipped";
    case RunOutcome.MISSED:
      return "missed";
    default:
      return "unknown";
  }
}

export interface DaemonLogsQuery {
  source?: string;
  level?: DaemonLogLevel;
  query?: string;
  since?: string;
  fromMs?: number | bigint;
  toMs?: number | bigint;
  limit?: number;
}

export interface ProfileQuery {
  query?: string;
  frameType?: string;
  comm?: string;
  fromMs?: number | bigint;
  toMs?: number | bigint;
  limit?: number;
}

export interface TraceOptions {
  kind: TraceKind;
  durationSeconds?: number;
  pid?: number;
  filter?: string;
  frequencyHz?: number;
}

export interface SampleOptions {
  durationSeconds?: number;
  pid?: number;
}

export interface StopOrPauseOptions {
  comment?: string;
  diagnose?: boolean;
}

export interface ResumeOptions {
  comment?: string;
}

export interface QuestionFilter {
  /** Only this project's; null or unset for every project's. */
  project?: string | null;
  /** None: every state but deleted. */
  states?: QuestionState[];
  order?: QuestionOrder;
}

export interface MemoryFilter {
  project?: string | null;
  scope?: MemoryScope;
  states?: MemoryState[];
  mode?: MemoryMode;
  query?: string;
  order?: MemoryOrder;
}

export interface ProposeMemoryInput {
  content: MemoryContentInput;
  global?: boolean;
  project?: string;
  supersedesIds?: number[];
  rationale?: string;
  approve?: boolean;
}

export interface ApproveMemoryInput {
  content?: MemoryContentInput;
  global?: boolean;
  project?: string;
  note?: string;
  ifRevision?: bigint;
}

export interface ReviseMemoryInput {
  content: MemoryContentInput;
  global?: boolean;
  project?: string;
  ifRevision?: bigint;
}

export interface Api {
  snapshot(): Promise<Snapshot>;
  steps(project: string): Promise<StepStatus[]>;
  commandLog(id: string): Promise<CommandLog>;
  files(workspaceId: string): Promise<FileEntry[]>;
  file(workspaceId: string, path: string): Promise<FileContent>;
  inspectCommand(id: string, opts?: { sections?: InspectSection[]; pid?: number }, signal?: AbortSignal): Promise<InspectCommandResponse>;
  traceCommand(id: string, opts: TraceOptions, onChunk: (chunk: TraceCommandChunk) => void, signal?: AbortSignal): Promise<TraceSummary | null>;
  sampleCommand(id: string, opts: SampleOptions, onChunk: (chunk: SampleCommandChunk) => void, signal?: AbortSignal): Promise<ProcessSample | null>;
  stopCommand(id: string, opts?: StopOrPauseOptions): Promise<Command>;
  pauseCommand(id: string, opts?: StopOrPauseOptions): Promise<Command>;
  resumeCommand(id: string, opts?: ResumeOptions): Promise<Command>;
  getCommandDiagnostics(id: string): Promise<CommandDiagnostics[]>;
  commandProfile(id: string, query?: ProfileQuery): Promise<CommandProfile>;
  nodeProfile(node: string, query?: ProfileQuery): Promise<CommandProfile>;
  daemonLogs(query?: DaemonLogsQuery): Promise<GetDaemonLogsResponse>;
  questions(filter: QuestionFilter): Promise<ListQuestionsResponse>;
  questionHistory(id: number): Promise<QuestionChange[]>;
  answerQuestion(id: number, answer: AnswerInput): Promise<Question>;
  dismissQuestion(id: number, reason: string): Promise<Question>;
  reviseQuestion(id: number, prompt: PromptInput): Promise<Question>;
  reopenQuestion(id: number): Promise<Question>;
  moveQuestion(id: number, before: number | null): Promise<Question>;
  memories(filter: MemoryFilter): Promise<ListMemoriesResponse>;
  memory(ref: string): Promise<Memory>;
  proposeMemory(input: ProposeMemoryInput): Promise<Memory>;
  approveMemory(id: number, input?: ApproveMemoryInput): Promise<Memory>;
  rejectMemory(id: number, reason: string, ifRevision?: bigint): Promise<Memory>;
  reviseMemory(id: number, input: ReviseMemoryInput): Promise<Memory>;
  archiveMemory(id: number, reason?: string, ifRevision?: bigint): Promise<Memory>;
  restoreMemory(id: number, toProposed?: boolean, ifRevision?: bigint): Promise<Memory>;
  moveMemory(id: number, before: number | null): Promise<Memory>;
  reviews(project: string | null): Promise<Review[]>;
  review(id: number): Promise<Review>;
  respond(reviewId: number, findingId: number | null, quick: QuickResponse, text: string): Promise<ReviewEvent>;
}
