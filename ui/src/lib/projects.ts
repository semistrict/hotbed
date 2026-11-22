import type { Project, Snapshot } from "../api";
import { workspaceRepo } from "./sessions";

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
 * commands and history; its repos' bases, scheduled commands and base
 * builds; the feed's lines about any of those. The machines (nodes, their
 * usage and the cluster's totals) are everyone's and stay whole. Null, or a
 * project the snapshot lacks, is every project: the snapshot as it is.
 */
export function scope(s: Snapshot, name: string | null): Snapshot {
  const project = s.projects.find((p) => p.name === name);
  if (!project) return s;
  const repos = projectRepoIds(s, project);
  const repoOf = workspaceRepo(s);
  const sessionProject = new Map(s.sessions.map((x) => [x.id, x.project]));
  const sessions = s.sessions.filter((x) => x.project === project.name || (x.project === null && x.workspaces.some((w) => repos.has(w.repoId))));
  const sessionIds = new Set(sessions.map((x) => x.id));
  const belongsTo = (directProject: string | null | undefined, sessionId: string, repoId: string) => {
    if (directProject) return directProject === project.name;
    if (sessionId) {
      const sp = sessionProject.get(sessionId);
      if (sp !== undefined && sp !== null) return sp === project.name;
      if (sessionIds.has(sessionId)) return true;
    }
    return repos.has(repoId);
  };
  const workspaces = s.workspaces.filter((w) => belongsTo(w.project, w.sessionId, w.repoId));
  const commands = s.commands.filter((c) => belongsTo(c.project, c.sessionId, repoOf(c.workspaceId)));
  // A base's build belongs to no session: it is its repo's.
  const history = s.history.filter((h) => belongsTo(h.project, h.sessionId, h.repoId));
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
    // A schedule's runs are in its project; one from before schedules had
    // one, in its repo's.
    schedules: s.schedules.filter((sc) => (sc.project !== null ? sc.project === project.name : repos.has(sc.repoId))),
    events: s.events.filter((e) => e.ref !== null && (e.ref.type === "command" ? commandIds.has(e.ref.id) : e.ref.type === "workspace" && workspaceIds.has(e.ref.id))),
    openQuestions: project.openQuestions,
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
