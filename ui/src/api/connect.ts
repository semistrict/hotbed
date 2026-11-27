import type { MessageInitShape } from "@bufbuild/protobuf";
import { createClient, type Transport } from "@connectrpc/connect";
import {
  BoardService,
  ClusterService,
  GitAuthService,
  IssueService,
  MemoryService,
  QuestionService,
  ReviewService,
  ScheduleService,
  SessionService,
  WorkspaceService,
  type CommandDiagnostics,
  type SampleCommandChunk,
  type SampleCommandRequestSchema,
  type TraceCommandChunk,
  type TraceCommandRequestSchema,
  type TraceSummary,
} from "../gen/hotbed/v1/hotbed_pb";

/** A Connect client for each of the controller's services. */
export function createClients(transport: Transport) {
  return {
    cluster: createClient(ClusterService, transport),
    sessions: createClient(SessionService, transport),
    workspaces: createClient(WorkspaceService, transport),
    schedules: createClient(ScheduleService, transport),
    questions: createClient(QuestionService, transport),
    reviews: createClient(ReviewService, transport),
    gitAuth: createClient(GitAuthService, transport),
    issues: createClient(IssueService, transport),
    memories: createClient(MemoryService, transport),
    board: createClient(BoardService, transport),
  };
}

export type Clients = ReturnType<typeof createClients>;

/**
 * What the cockpit reads from a command's streams and pages, gathered into one
 * answer: the parts of the protocol a single RPC call does not cover.
 */
export function commandStreams({ cluster, workspaces }: Pick<Clients, "cluster" | "workspaces">) {
  return {
    /** A command's output as written, from offset to what it has so far, and the offset to read on from. */
    async outputBytes(id: string, offset: bigint, signal?: AbortSignal): Promise<{ data: Uint8Array; next: bigint }> {
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

    /** Runs a trace, handing over each chunk; resolves to its last summary. */
    async traceCommand(
      req: MessageInitShape<typeof TraceCommandRequestSchema>,
      onChunk: (chunk: TraceCommandChunk) => void,
      signal?: AbortSignal,
    ): Promise<TraceSummary | null> {
      let summary: TraceSummary | null = null;
      for await (const ch of workspaces.traceCommand(req, { signal })) {
        if (ch.summary) summary = ch.summary;
        onChunk(ch);
      }
      return summary;
    },

    /** Takes a sample, kept with the command; resolves to its final chunk: the sample as kept, and its seq or why it was not kept. */
    async sampleCommand(
      req: MessageInitShape<typeof SampleCommandRequestSchema>,
      onChunk: (chunk: SampleCommandChunk) => void,
      signal?: AbortSignal,
    ): Promise<SampleCommandChunk | null> {
      let final: SampleCommandChunk | null = null;
      for await (const ch of workspaces.sampleCommand(req, { signal })) {
        if (ch.done) final = ch;
        onChunk(ch);
      }
      return final;
    },

    /** Every diagnostics record kept for a command, across its pages. */
    async getCommandDiagnostics(id: string): Promise<CommandDiagnostics[]> {
      const all: CommandDiagnostics[] = [];
      let afterSeq = 0;
      const maxPages = 64;
      for (let page = 0; page < maxPages; page++) {
        const res = await cluster.getCommandDiagnostics({ id, afterSeq });
        all.push(...res.diagnostics);
        // A cursor that does not move on would page forever.
        if (!res.hasMore || res.nextAfterSeq <= afterSeq) break;
        afterSeq = res.nextAfterSeq;
      }
      return all;
    },
  };
}

export type CommandStreams = ReturnType<typeof commandStreams>;
