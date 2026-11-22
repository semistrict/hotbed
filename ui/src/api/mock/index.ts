import type { Api, CommandLog, FileContent, FileEntry, Snapshot } from "../types";
import { mockFile, mockFiles } from "./files";
import { mockLog } from "./logs";
import { MockQuestions } from "./questions";
import { MockCluster } from "./sim";

/**
 * An Api backed by a cluster simulated in the browser. It catches up to the
 * wall clock on every call, so polling sees the cluster move.
 */
export function createMockApi(seed = 7120): Api {
  const cluster = new MockCluster(seed, Date.now());
  const questions = new MockQuestions(Date.now());
  const sync = () => cluster.advanceTo(Date.now());

  return {
    async snapshot(): Promise<Snapshot> {
      sync();
      const snap = cluster.snapshot();
      return {
        ...snap,
        openQuestions: questions.counts(null).open,
        projects: snap.projects.map((p) => ({ ...p, openQuestions: questions.counts(p.name).open, outbox: questions.counts(p.name).outbox })),
      };
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

    questions: async (filter) => questions.list(filter),
    questionHistory: async (id) => questions.history(id),
    answerQuestion: async (id, answer) => questions.answer(id, answer),
    dismissQuestion: async (id, reason) => questions.dismiss(id, reason),
    moveQuestion: async (id, before) => questions.move(id, before),
  };
}
