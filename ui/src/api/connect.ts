import { createClient, type Transport } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import {
  ActorKind,
  ClusterService,
  DaemonLogLevel,
  MemoryMode,
  MemoryOrder,
  MemoryScope,
  MemoryService,
  QuestionOrder,
  QuestionService,
  ReviewService,
  WorkspaceService,
} from "../gen/hotbed/v1/hotbed_pb";
import type { Api, CommandDiagnostics, DaemonLogsQuery, ProcessSample, ProfileQuery, SampleOptions, TraceOptions, TraceSummary } from "./types";

/**
 * The controller's API over Connect, from the page's own origin: the
 * controller serves this UI and the API side by side.
 */
export function createConnectApi(
  baseUrlOrTransport: string | Transport = typeof window !== "undefined" ? window.location.origin : "http://127.0.0.1:7120",
): Api {
  const transport = typeof baseUrlOrTransport === "string" ? createConnectTransport({ baseUrl: baseUrlOrTransport }) : baseUrlOrTransport;
  const cluster = createClient(ClusterService, transport);
  const workspaces = createClient(WorkspaceService, transport);
  const questions = createClient(QuestionService, transport);
  const memories = createClient(MemoryService, transport);
  const reviews = createClient(ReviewService, transport);

  return {
    snapshot: () => cluster.getSnapshot({}),
    steps: async (project) => (await cluster.listSteps({ project })).steps,
    commandLog: (id) => workspaces.getLogLines({ id, tail: 2000 }),
    files: async (workspaceId) => (await workspaces.listFiles({ workspaceId })).files,
    file: (workspaceId, path) => workspaces.getFile({ workspaceId, path }),
    inspectCommand: (id, opts, signal) =>
      workspaces.inspectCommand({ id, sections: opts?.sections ?? [], pid: opts?.pid ?? 0 }, { signal }),
    traceCommand: async (id, opts: TraceOptions, onChunk, signal) => {
      const stream = workspaces.traceCommand(
        {
          id,
          kind: opts.kind,
          durationSeconds: opts.durationSeconds ?? 5,
          pid: opts.pid ?? 0,
          filter: opts.filter ?? "",
          frequencyHz: opts.frequencyHz ?? 0,
        },
        { signal },
      );
      let finalSummary: TraceSummary | null = null;
      for await (const ch of stream) {
        if (ch.summary) finalSummary = ch.summary;
        onChunk(ch);
      }
      return finalSummary;
    },
    sampleCommand: async (id, opts: SampleOptions, onChunk, signal) => {
      const stream = workspaces.sampleCommand(
        {
          id,
          durationSeconds: opts.durationSeconds ?? 3,
          pid: opts.pid ?? 0,
        },
        { signal },
      );
      let finalSample: ProcessSample | null = null;
      for await (const ch of stream) {
        if (ch.sample) finalSample = ch.sample;
        onChunk(ch);
      }
      return finalSample;
    },
    stopCommand: (id, opts) =>
      workspaces.stopCommand({
        id,
        comment: opts?.comment ?? "",
        diagnose: opts?.diagnose ?? false,
      }),
    pauseCommand: (id, opts) =>
      workspaces.pauseCommand({
        id,
        comment: opts?.comment ?? "",
        diagnose: opts?.diagnose ?? false,
      }),
    resumeCommand: (id, opts) =>
      workspaces.resumeCommand({
        id,
        comment: opts?.comment ?? "",
      }),
    getCommandDiagnostics: async (id) => {
      const all: CommandDiagnostics[] = [];
      let afterSeq = 0;
      const maxPages = 64;
      for (let page = 0; page < maxPages; page++) {
        const res = await cluster.getCommandDiagnostics({ id, afterSeq });
        all.push(...res.diagnostics);
        if (!res.hasMore || res.nextAfterSeq <= afterSeq) {
          break;
        }
        afterSeq = res.nextAfterSeq;
      }
      return all;
    },
    commandProfile: (id, query?: ProfileQuery) =>
      workspaces.getProfile({
        target: { case: "commandId", value: id },
        query: query?.query ?? "",
        frameType: query?.frameType ?? "",
        comm: query?.comm ?? "",
        fromMs: BigInt(query?.fromMs ?? 0),
        toMs: BigInt(query?.toMs ?? 0),
        limit: query?.limit ?? 200,
      }),
    nodeProfile: (node, query?: ProfileQuery) =>
      workspaces.getProfile({
        target: { case: "node", value: node },
        query: query?.query ?? "",
        frameType: query?.frameType ?? "",
        comm: query?.comm ?? "",
        fromMs: BigInt(query?.fromMs ?? 0),
        toMs: BigInt(query?.toMs ?? 0),
        limit: query?.limit ?? 200,
      }),
    daemonLogs: (query?: DaemonLogsQuery) =>
      workspaces.getDaemonLogs({
        source: query?.source ?? "",
        level: query?.level ?? DaemonLogLevel.UNSPECIFIED,
        query: query?.query ?? "",
        since: query?.since ?? "",
        fromMs: BigInt(query?.fromMs ?? 0),
        toMs: BigInt(query?.toMs ?? 0),
        limit: query?.limit ?? 200,
      }),
    questions: ({ project, states = [], order = QuestionOrder.NEWEST }) =>
      questions.listQuestions({
        scope: project ? { case: "project", value: project } : { case: "allProjects", value: true },
        states,
        order,
      }),
    questionHistory: async (id) => (await questions.getQuestion({ id })).history,
    answerQuestion: (id, answer) => questions.answerQuestion({ id, answer }),
    dismissQuestion: (id, reason) => questions.dismissQuestion({ id, reason }),
    reviseQuestion: (id, prompt) => questions.reviseQuestion({ id, prompt }),
    reopenQuestion: (id) => questions.reopenQuestion({ id }),
    moveQuestion: (id, before) =>
      questions.moveQuestion({
        id,
        place: before === null ? { case: "last", value: true } : { case: "before", value: before },
      }),
    memories: ({ project, scope = MemoryScope.UNSPECIFIED, states = [], mode = MemoryMode.UNSPECIFIED, query = "", order = MemoryOrder.UNSPECIFIED }) =>
      memories.listMemories({
        projectScope:
          scope === MemoryScope.GLOBAL
            ? { case: undefined }
            : project
              ? { case: "project", value: project }
              : { case: "allProjects", value: true },
        scope,
        states,
        mode,
        query,
        order,
      }),
    memory: (ref) => memories.showMemory({ ref }),
    proposeMemory: (input) =>
      memories.proposeMemory({
        content: input.content,
        global: input.global ?? false,
        project: input.global ? "" : (input.project ?? ""),
        supersedesIds: input.supersedesIds ?? [],
        rationale: input.rationale ?? "",
        approve: input.approve ?? false,
      }),
    approveMemory: (id, input) =>
      memories.approveMemory({
        id,
        content: input?.content,
        global: input?.global ?? false,
        project: input?.global ? "" : (input?.project ?? ""),
        note: input?.note ?? "",
        ifRevision: input?.ifRevision ?? 0n,
      }),
    rejectMemory: (id, reason, ifRevision = 0n) => memories.rejectMemory({ id, reason, ifRevision }),
    reviseMemory: (id, input) =>
      memories.reviseMemory({
        id,
        content: input.content,
        global: input.global ?? false,
        project: input.global ? "" : (input.project ?? ""),
        ifRevision: input.ifRevision ?? 0n,
      }),
    archiveMemory: (id, reason = "", ifRevision = 0n) => memories.archiveMemory({ id, reason, ifRevision }),
    restoreMemory: (id, toProposed = false, ifRevision = 0n) => memories.restoreMemory({ id, toProposed, ifRevision }),
    moveMemory: (id, before) =>
      memories.moveMemory({
        id,
        place: before === null ? { case: "last", value: true } : { case: "before", value: before },
      }),
    reviews: async (project) =>
      (await reviews.listReviews({ scope: project ? { case: "project", value: project } : { case: "allProjects", value: true } })).reviews,
    review: (id) => reviews.getReview({ id }),
    respond: (reviewId, findingId, quick, text) =>
      reviews.respond({ reviewId, findingId: findingId ?? 0, quick, text, actorKind: ActorKind.USER }),
  };
}
