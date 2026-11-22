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

/**
 * The cluster as one project sees it: its sessions and their workspaces,
 * commands and history; its repos' bases, scheduled commands and base
 * builds; the feed's lines about any of those. The machines (nodes, their
 * usage and the cluster's totals) are everyone's and stay whole. Null, or a
 * project the snapshot lacks, is every project: the snapshot as it is.
 */
export function scope(s: Snapshot, name: string | null): Snapshot {
  const project = s.projects.find((p) => p.name === name);
  if (!project) return s;
  const repos = projectRepoIds(s, project);
  const sessions = s.sessions.filter((x) => x.project === project.name);
  const sessionIds = new Set(sessions.map((x) => x.id));
  const workspaces = s.workspaces.filter((w) => sessionIds.has(w.sessionId));
  const commands = s.commands.filter((c) => sessionIds.has(c.sessionId));
  // A base's build belongs to no session: it is its repo's.
  const history = s.history.filter((h) => (h.sessionId ? sessionIds.has(h.sessionId) : repos.has(h.repoId)));
  const workspaceIds = new Set(workspaces.map((w) => w.id));
  const commandIds = new Set([...commands.map((c) => c.id), ...history.map((h) => h.id)]);
  return {
    ...s,
    repos: s.repos.filter((r) => repos.has(r.id)),
    bases: s.bases.filter((b) => repos.has(b.repoId)),
    sessions,
    workspaces,
    commands,
    history,
    schedules: s.schedules.filter((sc) => repos.has(sc.repoId)),
    events: s.events.filter((e) => e.ref !== null && (e.ref.type === "command" ? commandIds.has(e.ref.id) : e.ref.type === "workspace" && workspaceIds.has(e.ref.id))),
    openQuestions: project.openQuestions,
  };
}

/** The project a session's command belongs to; a base build's (no session) is its repo's. */
export function commandProject(s: Snapshot, sessionId: string, repoId: string | null): string | null {
  if (sessionId) return s.sessions.find((x) => x.id === sessionId)?.project ?? null;
  const remote = s.repos.find((r) => r.id === repoId)?.remote;
  return s.projects.find((p) => remote !== undefined && p.repos.includes(remote))?.name ?? null;
}
