import { createClient, type Transport } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import {
  ActorKind,
  BoardService,
  ClusterService,
  DaemonLogLevel,
  GitAuthService,
  MemoryMode,
  MemoryOrder,
  MemoryScope,
  MemoryService,
  QuestionOrder,
  QuestionService,
  ReviewService,
  IssueService,
  WorkspaceService,
} from "../gen/hotbed/v1/hotbed_pb";
import type { Api, CommandDiagnostics, DaemonLogsQuery, ProfileQuery, SampleCommandChunk, SampleOptions, TraceOptions, TraceSummary } from "./types";

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
  const issues = createClient(IssueService, transport);
  const gitauth = createClient(GitAuthService, transport);
  const board = createClient(BoardService, transport);

  return {
    snapshot: () => cluster.getSnapshot({}),
    retireNode: (node) => cluster.retireNode({ node }),
    steps: async (project) => (await cluster.listSteps({ project })).steps,
    getWorkerConfig: (node = "") => cluster.getWorkerConfig({ node }),
    setWorkerConfig: (input) =>
      cluster.setWorkerConfig({
        node: input.node ?? "",
        config: input.config,
        replace: input.replace ?? false,
        ifRevision: input.ifRevision ?? 0n,
      }),
    deleteWorkerConfig: (input) =>
      cluster.deleteWorkerConfig({
        node: input.node ?? "",
        defaults: input.defaults ?? false,
      }),
    upgradeSystem: (node = "") => cluster.upgradeSystem({ node }),
    listGitCredentials: async (host = "") => (await gitauth.listGitCredentials({ host })).credentials,
    putGitCredential: (input) =>
      gitauth.putGitCredential({
        name: input.name,
        host: input.host ?? "github.com",
        repos: input.repos ?? [],
        token: input.token,
        username: input.username ?? "",
        apiUrl: input.apiUrl ?? "",
      }),
    deleteGitCredential: (name) => gitauth.deleteGitCredential({ name }),
    startGitHubLogin: (input) =>
      gitauth.startGitHubLogin({
        name: input?.name ?? "github",
        host: input?.host ?? "github.com",
        repos: input?.repos ?? [],
        scopes: input?.scopes ?? [],
        clientId: input?.clientId ?? "",
        apiUrl: input?.apiUrl ?? "",
        oauthUrl: input?.oauthUrl ?? "",
      }),
    completeGitHubLogin: (loginId) => gitauth.completeGitHubLogin({ loginId }),
    commandLog: (id) => workspaces.getLogLines({ id, tail: 2000 }),
    outputBytes: async (id, offset, signal) => {
      const parts: Uint8Array[] = [];
      let next = offset;
      for await (const ch of workspaces.followLog({ id, offset, follow: false }, { signal })) {
        if (ch.data.length > 0) parts.push(ch.data);
        next = ch.nextOffset;
      }
      const data = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
      let at = 0;
      for (const p of parts) {
        data.set(p, at);
        at += p.length;
      }
      return { data, next };
    },
    queryOutput: (id, query) => workspaces.queryOutput({ ...query, id }),
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
      let final: SampleCommandChunk | null = null;
      for await (const ch of stream) {
        if (ch.done) final = ch;
        onChunk(ch);
      }
      return final;
    },
    listCommandSamples: async (id) => (await cluster.listCommandSamples({ id })).samples,
    getCommandSample: (id, seq) => cluster.getCommandSample({ id, seq }),
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
    question: (id) => questions.getQuestion({ id }),
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
        repo: input.global ? "" : (input.repo ?? ""),
        supersedesIds: input.supersedesIds ?? [],
        rationale: input.rationale ?? "",
      }),
    reviseMemory: (id, input) =>
      memories.reviseMemory({
        id,
        content: input.content,
        global: input.global ?? false,
        project: input.global ? "" : (input.project ?? ""),
        repo: input.repo,
        rationale: input.rationale,
        replaceContent: input.replaceContent ?? false,
        clearTitle: input.clearTitle ?? false,
        clearBody: input.clearBody ?? false,
        replaceTags: input.replaceTags ?? false,
        ifRevision: input.ifRevision ?? 0n,
      }),
    archiveMemory: (id, reason = "", ifRevision = 0n) => memories.archiveMemory({ id, reason, ifRevision }),
    restoreMemory: (id, ifRevision = 0n) => memories.restoreMemory({ id, ifRevision }),
    deleteMemory: (id, ifRevision = 0n) => memories.deleteMemory({ id, ifRevision }),
    moveMemory: (id, before) =>
      memories.moveMemory({
        id,
        place: before === null ? { case: "last", value: true } : { case: "before", value: before },
      }),
    issues: async (which) =>
      (await issues.listIssues({ archived: which === "archived", all: which === "all" })).issues,
    issue: (id) => issues.getIssue({ id }),
    archiveIssue: (id, reason) => issues.archiveIssue({ id, reason }),
    reopenIssue: (id) => issues.reopenIssue({ id }),
    reviews: async (project) =>
      (await reviews.listReviews({ scope: project ? { case: "project", value: project } : { case: "allProjects", value: true } })).reviews,
    review: (id) => reviews.getReview({ id }),
    respond: (reviewId, findingId, quick, text) =>
      reviews.respond({ reviewId, findingId: findingId ?? 0, quick, text, actorKind: ActorKind.USER }),
    boardThreads: ({ project, text = "", by = "", mentions = "", labels = [], before = "", limit = 50 }) =>
      board.listBoardThreads({
        scope: project ? { case: "project", value: project } : { case: "allProjects", value: true },
        text,
        by,
        mentions,
        labels: labels.map((l) => ({ key: l.key, values: l.values ?? [] })),
        before,
        limit,
      }),
    boardThread: (id) => board.getBoardThread({ id }),
    postBoardMessage: (input) =>
      board.postBoardMessage({
        project: input.project ?? "",
        re: input.re ?? 0n,
        text: input.text,
        labels: input.labels ?? {},
        attachmentIds: input.attachmentIds ?? [],
      }),
    editBoardMessage: (id, text, ifRevision = 0n) => board.editBoardMessage({ id, text, ifRevision }),
    deleteBoardMessage: (id) => board.deleteBoardMessage({ id }),
    reactBoardMessage: (id, emoji, remove = false) => board.reactBoardMessage({ id, emoji, remove }),
    labelBoardMessage: (id, set, unset = []) => board.labelBoardMessage({ id, set, unset }),
    uploadBoardAttachment: (name, contentType, data) => board.uploadBoardAttachment({ name, contentType, data }),
    boardTasks: ({ project, states = [], includeAll = false, labels = [], owner = "", forActor = "", latest = false, limit = 100 }) =>
      board.listBoardTasks({
        scope: project ? { case: "project", value: project } : { case: "allProjects", value: true },
        states,
        includeAll,
        labels: labels.map((l) => ({ key: l.key, values: l.values ?? [] })),
        owner,
        forActor,
        latest,
        limit,
      }),
    createBoardTask: (input) =>
      board.createBoardTask({
        project: input.project ?? "",
        re: input.re ?? 0n,
        text: input.text ?? "",
        labels: input.labels ?? {},
        attachmentIds: input.attachmentIds ?? [],
        needs: input.needs ?? [],
        forActor: input.forActor ?? "",
      }),
    blockBoardTask: (id, reason, kind = "") => board.blockBoardTask({ id, reason, kind }),
    unblockBoardTask: (id) => board.unblockBoardTask({ id }),
    reopenBoardTask: (id) => board.reopenBoardTask({ id }),
    assignBoardTask: (id, forActor, unassign = false) => board.assignBoardTask({ id, forActor, unassign }),
    cancelBoardTask: (id, reason) => board.cancelBoardTask({ id, reason }),
    boardSchedules: (project, includeAll = false) =>
      board.listBoardSchedules({
        scope: project ? { case: "project", value: project } : { case: "allProjects", value: true },
        includeAll,
      }),
    cancelBoardSchedule: async (id) => (await board.cancelBoardSchedule({ id })).schedule!,
  };
}
