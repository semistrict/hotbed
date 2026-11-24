import type { Project, Snapshot } from "../api";

/** A canonical remote as people say it: github.com/semistrict/hotbed → semistrict/hotbed. */
export function remoteName(remote: string): string {
  const parts = remote.split("/");
  return parts.length > 2 ? parts.slice(1).join("/") : remote;
}

/** The hotbed repos (by ID) a project holds: those whose remote it names. */
export function projectRepoIds(s: Snapshot, project: Project): Set<string> {
  return new Set(s.repos.filter((r) => project.repos.includes(r.remote)).map((r) => r.id));
}

/** The project holding a repo, if any. */
export function repoProject(s: Snapshot, repoId: string | null | undefined): string | null {
  if (!repoId) return null;
  const remote = s.repos.find((r) => r.id === repoId)?.remote;
  return s.projects.find((p) => remote !== undefined && p.repos.includes(remote))?.name ?? null;
}

/**
 * The cluster as one project sees it: its sessions and their workspaces,
 * commands and history; its bases, scheduled commands and base builds; the
 * feed's lines about any of those. The machines (nodes, their usage and the
 * cluster's totals) are everyone's and stay whole. Null, or a project the
 * snapshot lacks, is every project: the snapshot as it is.
 */
export function scope(s: Snapshot, name: string | null): Snapshot {
  const project = s.projects.find((p) => p.name === name);
  if (!project) return s;
  const repos = projectRepoIds(s, project);
  const sessionProject = new Map(s.sessions.map((x) => [x.id, x.project]));
  const sessions = s.sessions.filter((x) => x.project === project.name);
  // A command belongs to its project, or its session's; a base's build,
  // which no session has, to its repo's.
  const belongsTo = (directProject: string | null | undefined, sessionId: string, repoId: string | null) => {
    if (directProject) return directProject === project.name;
    const sp = sessionId ? sessionProject.get(sessionId) : "";
    if (sp) return sp === project.name;
    return Boolean(repoId) && repos.has(repoId!);
  };
  const workspaces = s.workspaces.filter((w) => w.project === project.name);
  const commands = s.commands.filter((c) => belongsTo(c.project, c.sessionId, c.repo));
  const history = s.history.filter((h) => belongsTo(h.project, h.sessionId, h.repoId));
  const workspaceIds = new Set(workspaces.map((w) => w.id));
  const commandIds = new Set([...commands.map((c) => c.id), ...history.map((h) => h.id)]);
  return {
    ...s,
    repos: s.repos.filter((r) => repos.has(r.id)),
    bases: s.bases.filter((b) => b.project === project.name),
    sessions,
    workspaces,
    commands,
    history,
    schedules: s.schedules.filter((sc) => sc.project === project.name),
    events: s.events.filter((e) => e.ref !== undefined && (e.ref.type === "command" ? commandIds.has(e.ref.id) : e.ref.type === "workspace" && workspaceIds.has(e.ref.id))),
    openQuestions: project.openQuestions,
    pendingMemories: s.globalPendingMemories + project.pendingMemories,
  };
}

/** The project a command belongs to: its session's, or its repo's when the session has none. */
export function commandProject(s: Snapshot, sessionId: string, repoId: string | null): string | null {
  if (sessionId) {
    const p = s.sessions.find((x) => x.id === sessionId)?.project;
    if (p) return p;
  }
  return repoProject(s, repoId);
}
