// The workspaces table: which rows a reader asked for, and in what order.
import type { Workspace } from "../api";

export interface WorkspaceFilter {
  /** Only this repo's workspaces. */
  repo?: string;
  /** Text to find in a workspace's id, agent, session, branch or node. */
  q?: string;
}

/**
 * The workspaces matching f: live ones first, then released, each by most
 * recent activity. nodeName turns a node id into the hostname people search for.
 */
export function filterWorkspaces(all: readonly Workspace[], f: WorkspaceFilter, nodeName: (id: string) => string): Workspace[] {
  const q = f.q?.trim().toLowerCase() ?? "";
  return all
    .filter((w) => !f.repo || w.repoId === f.repo)
    .filter((w) => q === "" || [w.id, w.agent, w.sessionId, w.branch, nodeName(w.nodeId)].some((x) => x.toLowerCase().includes(q)))
    .sort((a, b) => Number(a.state === "released") - Number(b.state === "released") || b.lastActiveAt.localeCompare(a.lastActiveAt));
}
