// Hotbed API types: every generated protobuf-es message, schema, enum and
// service from hotbed.proto, plus the cockpit's enum slugs and labels.

import {
  type Admission,
  BaseState,
  CommandState,
  DaemonLogLevel,
  MemoryOrder,
  type Node,
  QuestionOrder,
  RunOutcome,
  SkillOrder,
  StepState,
  WorkspaceState,
} from "../gen/hotbed/v1/hotbed_pb";

export type { MessageInitShape } from "@bufbuild/protobuf";
export * from "../gen/hotbed/v1/hotbed_pb";

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

export type SkillOrderSlug = "rank" | "newest" | "oldest" | "updated" | "name";
export const SKILL_ORDER_FROM_SLUG: Record<SkillOrderSlug, SkillOrder> = {
  rank: SkillOrder.RANK,
  newest: SkillOrder.NEWEST,
  oldest: SkillOrder.OLDEST,
  updated: SkillOrder.UPDATED,
  name: SkillOrder.NAME,
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
