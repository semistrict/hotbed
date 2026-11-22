import type { Api, CommandLog, CommandProfile, FileContent, FileEntry, InspectCommandResult, InspectSection, ProfileQuery, Snapshot, TraceChunk, TraceOptions, TraceSummary } from "../types";
import { mockCommandProfile, mockInspect, mockTrace } from "./diagnostics";
import { mockFile, mockFiles } from "./files";
import { mockLog } from "./logs";
import { MockCluster } from "./sim";

/**
 * An Api backed by a cluster simulated in the browser. It catches up to the
 * wall clock on every call, so polling sees the cluster move.
 */
export function createMockApi(seed = 7120): Api {
  const cluster = new MockCluster(seed, Date.now());
  const sync = () => cluster.advanceTo(Date.now());

  return {
    async snapshot(): Promise<Snapshot> {
      sync();
      return cluster.snapshot();
    },

    async commandLog(id: string): Promise<CommandLog> {
      sync();
      const { command, finished } = cluster.findCommand(id);
      if (command) {
        const ws = cluster.findWorkspace(command.workspaceId);
        const started = Date.parse(command.startedAt ?? command.queuedAt);
        const ended = command.endedAt ? Date.parse(command.endedAt) : Date.now();
        return {
          commandId: id,
          lines: mockLog({
            id,
            repoId: ws?.repoId ?? "hotbed",
            argv: command.argv,
            kind: command.kind,
            state: command.state,
            rebuilt: command.rebuilt?.units ?? 0,
            seconds: Math.max(0, (ended - started) / 1000),
            failure: command.state === "failed" ? (command.exitCode === 100 ? "test" : "compile") : null,
            url: command.url,
          }),
        };
      }
      if (finished) {
        return {
          commandId: id,
          lines: mockLog({ ...finished, rebuilt: finished.rebuilt ?? 0, url: null }),
        };
      }
      throw new Error(`no command ${id}`);
    },

    async files(workspaceId: string): Promise<FileEntry[]> {
      const w = cluster.findWorkspace(workspaceId);
      if (!w) throw new Error(`no workspace ${workspaceId}`);
      return mockFiles(w);
    },

    async file(workspaceId: string, path: string): Promise<FileContent> {
      const w = cluster.findWorkspace(workspaceId);
      if (!w) throw new Error(`no workspace ${workspaceId}`);
      return mockFile(w, path);
    },

    async inspectCommand(id: string, opts?: { sections?: InspectSection[]; pid?: number }): Promise<InspectCommandResult> {
      sync();
      const { command } = cluster.findCommand(id);
      if (!command) throw new Error(`command ${id} is not running`);
      return mockInspect(id, command.argv, opts);
    },

    async traceCommand(id: string, opts: TraceOptions, onChunk: (chunk: TraceChunk) => void, signal?: AbortSignal): Promise<TraceSummary | null> {
      sync();
      const { command } = cluster.findCommand(id);
      if (!command) throw new Error(`command ${id} is not running`);
      return mockTrace(id, opts, onChunk, signal);
    },

    async commandProfile(id: string, query?: ProfileQuery): Promise<CommandProfile> {
      sync();
      const { command, finished } = cluster.findCommand(id);
      if (command) {
        const started = Date.parse(command.startedAt ?? command.queuedAt);
        const ended = command.endedAt ? Date.parse(command.endedAt) : null;
        return mockCommandProfile(id, command.argv, ended === null, started, ended, query);
      }
      if (finished) {
        const started = finished.endedAt - finished.seconds * 1000;
        return mockCommandProfile(id, finished.argv, false, started, finished.endedAt, query);
      }
      throw new Error(`no command ${id}`);
    },
  };
}
