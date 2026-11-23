// The workspaces table: which rows a reader asked for, and in what order.
import { WorkspaceState, type Workspace } from "../api";
import { cmpBig } from "./format";

export interface WorkspaceFilter {
  /** Only this project's workspaces. */
  project?: string;
  /** Text to find in a workspace's id, agent, session, repos or node. */
  q?: string;
}

/** Files a workspace's syncs changed, over all its repos. */
export function filesChanged(w: Workspace): number {
  return w.repos.reduce((n, r) => n + r.filesChanged, 0);
}

/**
 * The workspaces matching f: live ones first, then released, each by most
 * recent activity. nodeName turns a node id into the hostname people search for.
 */
export function filterWorkspaces(all: readonly Workspace[], f: WorkspaceFilter, nodeName: (id: string) => string): Workspace[] {
  const q = f.q?.trim().toLowerCase() ?? "";
  return all
    .filter((w) => !f.project || w.project === f.project)
    .filter((w) => q === "" || [w.id, w.agent, w.sessionId, ...w.repos.map((r) => r.repo), nodeName(w.nodeId)].some((x) => x.toLowerCase().includes(q)))
    .sort((a, b) => Number(a.state === WorkspaceState.RELEASED) - Number(b.state === WorkspaceState.RELEASED) || cmpBig(b.lastActiveAt, a.lastActiveAt));
}
