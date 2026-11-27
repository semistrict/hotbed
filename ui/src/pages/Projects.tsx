import { Link, useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { Input } from "@/components/ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { BaseState, CommandState, WorkspaceState, workspaceStateLabel, type Project, type RepoSummary, type Snapshot, type Workspace } from "../api";
import { SessionLink } from "../components/SessionLink";
import { SetupSteps } from "../components/SetupSteps";
import { Card, Empty, StatTile } from "../components/Panel";
import { nodeName, useSnapshot } from "../lib/data";
import { ago, bytes, compact, duration, percent, quantile } from "../lib/format";
import { isLive } from "../lib/live";
import { useProject } from "../lib/project";
import { remoteName, scope } from "../lib/projects";
import { sessionState } from "../lib/sessions";
import { filesChanged, filterWorkspaces } from "../lib/workspaces";
import { PageTitle } from "../components/PageTitle";
import { stateTone } from "./Workspace";

export function ProjectsPage() {
  const { data: s } = useSnapshot();
  if (!s) return null;
  return <Projects s={s} />;
}

/** What a project has going on, from the snapshot as it sees it. */
interface Activity {
  project: Project;
  working: number;
  idle: number;
  running: number;
  finished: number;
  failed: number;
  liveWorkspaces: number;
  ownedBytes: number;
  /** When one of its sessions was last touched; 0 for never. */
  lastActive: number;
}

function activity(s: Snapshot, project: Project): Activity {
  const mine = scope(s, project.name);
  const live = mine.sessions.filter((x) => x.endedAt === undefined);
  const working = live.filter((x) => sessionState(mine, x) === "working").length;
  const liveWs = mine.workspaces.filter((w) => w.state !== WorkspaceState.RELEASED);
  return {
    project,
    working,
    idle: live.length - working,
    running: mine.commands.filter(isLive).length,
    finished: mine.history.length,
    failed: mine.history.filter((c) => c.state === CommandState.FAILED).length,
    liveWorkspaces: liveWs.length,
    ownedBytes: liveWs.reduce((n, w) => n + Number(w.exclusiveBytes), 0),
    lastActive: Math.max(0, ...mine.sessions.map((x) => Number(x.lastActiveAt))),
  };
}

// What needs the user first (questions), then what is busy, then what holds workspaces, then most recently touched.
function urgency(a: Activity): number {
  return (a.project.openQuestions > 0 ? 8 : 0) + (a.working > 0 ? 4 : 0) + (a.liveWorkspaces > 0 ? 2 : 0) + (a.idle > 0 ? 1 : 0);
}

function Projects({ s }: { s: Snapshot }) {
  const [chosen] = useProject();
  const [q, setQ] = useState("");
  const now = Number(s.now);
  const visibleProjects = chosen ? s.projects.filter((p) => p.name === chosen) : s.projects;
  const all = visibleProjects
    .map((p) => activity(s, p))
    .sort((a, b) => urgency(b) - urgency(a) || b.lastActive - a.lastActive || a.project.name.localeCompare(b.project.name));

  const scoped = chosen ? scope(s, chosen) : s;
  const liveWs = scoped.workspaces.filter((w) => w.state !== WorkspaceState.RELEASED);
  const releasedWs = scoped.workspaces.length - liveWs.length;
  const forkMs = liveWs.map((w) => Number(w.forkMillis));
  const owned = liveWs.reduce((n, w) => n + Number(w.exclusiveBytes), 0);
  const liveSessions = all.reduce((n, a) => n + a.working + a.idle, 0);
  const workingProjects = all.filter((a) => a.working > 0).length;
  const outbox = visibleProjects.reduce((n, p) => n + p.outbox, 0);
  const needle = q.trim().toLowerCase();

  const filtered = needle
    ? all.filter((a) => {
        if (a.project.name.toLowerCase().includes(needle)) return true;
        if (a.project.repos.some((r) => r.toLowerCase().includes(needle))) return true;
        const mine = scope(s, a.project.name);
        return filterWorkspaces(mine.workspaces, { q: needle }, (id) => nodeName(s, id)).length > 0;
      })
    : all;

  return (
    <div className="space-y-6">
      <PageTitle
        title={chosen ? chosen : "Projects"}
        sub={
          chosen
            ? "Repos, warm bases and worker workspaces for this project. Choose every project in the picker to see the whole cluster."
            : "Every project, its repos, and the workspaces workers keep for them. Registering a repo makes a project for it automatically."
        }
      />

      <div className={`grid grid-cols-2 gap-2.5 sm:grid-cols-3 sm:gap-3 ${chosen ? "xl:grid-cols-5" : "xl:grid-cols-6"}`}>
        {!chosen && <StatTile label="Projects" value={visibleProjects.length} foot={`${workingProjects} working now`} />}
        <StatTile label="Repos" value={scoped.repos.length} foot={`${scoped.bases.filter((b) => b.state === BaseState.WARM).length} warm bases`} />
        <StatTile
          label="Workspaces"
          value={liveWs.length}
          foot={
            liveWs.length > 0
              ? `${bytes(owned)} owned · p50 ${quantile(forkMs, 0.5)} ms fork`
              : chosen
                ? releasedWs > 0
                  ? `${releasedWs} released`
                  : "none live right now"
                : `${compact(s.totals24h?.forks ?? 0)} forks today`
          }
        />
        <StatTile label="Live sessions" value={liveSessions} tone="ember" foot={chosen ? `in ${chosen}` : "across every project"} />
        <StatTile label="Questions" value={scoped.openQuestions} tone={scoped.openQuestions > 0 ? "ember" : "default"} foot={`${outbox} in outbox`} />
        <StatTile
          label="Commands today"
          value={compact(all.reduce((n, a) => n + a.finished, 0))}
          foot={`${all.reduce((n, a) => n + a.failed, 0)} failed`}
        />
      </div>

      {all.length > 0 && (
        <div className="rise flex flex-wrap items-center gap-2">
          <Input
            type="search"
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder={chosen ? "filter repos, workspaces, sessions or nodes" : "filter projects, repos, workspaces, sessions or nodes"}
            aria-label={chosen ? `Filter ${chosen} repos and workspaces` : "Filter projects and workspaces"}
            className="w-full rounded-full bg-background/40 font-mono text-[11px] placeholder:text-faint sm:w-96"
          />
          {q && !chosen && (
            <span className="font-mono text-[11px] text-faint">
              {filtered.length} of {all.length} projects
            </span>
          )}
        </div>
      )}

      {all.length === 0 ? (
        <Card>
          <Empty>No projects yet. Adding a repo or starting a session makes one; hotbed project add NAME --repo REMOTE groups several repos.</Empty>
        </Card>
      ) : filtered.length === 0 ? (
        <Card>
          <Empty>No project, repo or workspace matches “{q}”.</Empty>
        </Card>
      ) : (
        <div className="space-y-5">
          {filtered.map((a, i) => (
            <ProjectSection key={a.project.name} s={s} a={a} now={now} q={needle} delay={40 * Math.min(i, 6)} />
          ))}
        </div>
      )}
    </div>
  );
}

function ProjectSection({ s, a, now, q, delay }: { s: Snapshot; a: Activity; now: number; q: string; delay: number }) {
  const [chosen, choose] = useProject();
  const navigate = useNavigate();
  const p = a.project;
  const here = chosen === p.name;
  const open = (to: "/sessions" | "/commands" | "/questions" | "/reviews" | "/schedules") => {
    choose(p.name);
    void navigate({ to });
  };
  const projectMatches = q !== "" && p.name.toLowerCase().includes(q);

  return (
    <Card delay={delay} className={here ? "border-primary/50" : ""}>
      <div className="space-y-4">
        <header className="flex flex-col gap-3 border-b border-border pb-3 lg:flex-row lg:flex-wrap lg:items-baseline lg:gap-x-4 lg:gap-y-2">
          <div className="flex flex-wrap items-baseline gap-x-2.5 gap-y-1">
            <button
              type="button"
              onClick={() => choose(here ? null : p.name)}
              title={here ? "Show every project" : `Scope cockpit to ${p.name}`}
              className="display text-left text-[24px] leading-none font-medium hover:text-primary sm:text-[26px]"
            >
              {p.name}
            </button>
            <span className="font-mono text-[11px] text-faint">{a.lastActive > 0 ? `active ${ago(a.lastActive, now)}` : "no sessions today"}</span>
          </div>

          <div className="flex flex-wrap items-center gap-2 font-mono text-[11px] lg:ml-auto lg:gap-2.5">
            <button type="button" onClick={() => open("/sessions")} className="rounded-md border border-border px-2.5 py-1.5 text-muted-foreground hover:border-border-strong hover:text-foreground sm:py-1">
              <span className={a.working > 0 ? "text-primary" : "text-foreground"}>{a.working + a.idle}</span> sessions
              {a.working > 0 && <span className="text-primary"> · {a.working} working</span>}
            </button>
            <button type="button" onClick={() => open("/commands")} className="rounded-md border border-border px-2.5 py-1.5 text-muted-foreground hover:border-border-strong hover:text-foreground sm:py-1">
              {a.running > 0 && <span className="text-primary">{a.running} live · </span>}
              <span className="text-foreground">{a.finished}</span> today
              {a.failed > 0 && <span className="text-critical"> · {a.failed} failed</span>}
            </button>
            <button type="button" onClick={() => open("/questions")} className="rounded-md border border-border px-2.5 py-1.5 text-muted-foreground hover:border-border-strong hover:text-foreground sm:py-1">
              <span className={p.openQuestions > 0 ? "text-primary" : "text-foreground"}>{p.openQuestions}</span> questions
              {p.outbox > 0 && <span className="text-frost"> · {p.outbox} outbox</span>}
            </button>
            <button type="button" onClick={() => open("/reviews")} className="rounded-md border border-border px-2.5 py-1.5 text-muted-foreground hover:border-border-strong hover:text-foreground sm:py-1">
              <span className={p.activeReviews > 0 ? "text-primary" : "text-foreground"}>{p.activeReviews}</span> reviews
              {p.reviewsToDecide > 0 && <span className="text-primary"> · {p.reviewsToDecide} to decide</span>}
            </button>
            <button type="button" onClick={() => open("/schedules")} className="rounded-md border border-border px-2.5 py-1.5 text-muted-foreground hover:border-border-strong hover:text-foreground sm:py-1">
              <span className="text-foreground">{p.schedules}</span> schedules
            </button>
          </div>
        </header>

        <SetupSteps project={p} now={now} />
        <ProjectBody s={s} project={p} now={now} q={projectMatches ? "" : q} />
      </div>
    </Card>
  );
}

/**
 * A project's repos, checked out side by side in each of its workspaces, its
 * warm bases, and the workspaces its sessions hold.
 */
function ProjectBody({ s, project, now, q }: { s: Snapshot; project: Project; now: number; q: string }) {
  const [showReleased, setShowReleased] = useState(false);
  const bases = s.bases.filter((b) => b.project === project.name && b.state !== BaseState.STALE);
  const repoMatches = q !== "" && project.repos.some((r) => r.toLowerCase().includes(q));
  const allWorkspaces = filterWorkspaces(s.workspaces, { project: project.name, q: repoMatches ? "" : q }, (id) => nodeName(s, id));
  const live = allWorkspaces.filter((w) => w.state !== WorkspaceState.RELEASED);
  const released = allWorkspaces.filter((w) => w.state === WorkspaceState.RELEASED);
  const owned = live.reduce((n, w) => n + Number(w.exclusiveBytes), 0);
  const shown = showReleased || q !== "" ? allWorkspaces : live;

  return (
    <div className="rounded-xl border border-border/80 bg-background/35 p-3 sm:p-4">
      {project.repos.length === 0 ? (
        <p className="font-mono text-[11.5px] text-faint">No repos in this project yet. Add one with hotbed project add {project.name} --repo REMOTE.</p>
      ) : (
        <ul className="space-y-1">
          {project.repos.map((remote) => (
            <RepoLine key={remote} s={s} remote={remote} />
          ))}
        </ul>
      )}
      <div className="mt-2.5 flex flex-wrap items-baseline gap-x-3 gap-y-1 border-t border-border/60 pt-2.5">
        <BasesSummary s={s} bases={bases} />
        <span className="font-mono text-[11px] text-muted-foreground sm:ml-auto">
          {live.length > 0 ? (
            <>
              <span className="text-foreground">{live.length}</span> live workspace{live.length === 1 ? "" : "s"} · {bytes(owned)} owned
            </>
          ) : (
            <span className="text-faint">no live workspaces</span>
          )}
          {released.length > 0 && q === "" && (
            <button
              type="button"
              onClick={() => setShowReleased((v) => !v)}
              className="ml-2.5 text-faint underline-offset-2 hover:text-primary hover:underline"
            >
              {showReleased ? "hide released" : `+${released.length} released`}
            </button>
          )}
        </span>
      </div>
      {shown.length > 0 && (
        <div className="mt-3 border-t border-border/60 pt-2">
          <RepoWorkspaceTable s={s} rows={shown} now={now} />
        </div>
      )}
    </div>
  );
}

/** One repo of a project: where its checkout sits, and what it builds. */
function RepoLine({ s, remote }: { s: Snapshot; remote: string }) {
  const repo: RepoSummary | undefined = s.repos.find((x) => x.remote === remote);
  return (
    <li className="flex flex-wrap items-baseline gap-x-3 gap-y-0.5">
      <span className="font-mono text-[13px] font-medium text-foreground" title={remote}>
        {remoteName(remote)}
      </span>
      {repo ? (
        <span className="font-mono text-[11px] text-muted-foreground">
          /work/{repo.id} · {repo.language}
          {repo.units > 0 && ` · ${repo.units} ${repo.language === "rust" ? "crates" : "packages"}`}
          {repo.coldBuildSeconds > 0 && ` · cold ${duration(repo.coldBuildSeconds)}`}
        </span>
      ) : (
        <span className="font-mono text-[11px] text-critical" title="Workspaces of the project cannot be made until it is: hotbed repo add NAME REMOTE">
          not kept by hotbed
        </span>
      )}
    </li>
  );
}

function BasesSummary({ s, bases }: { s: Snapshot; bases: Snapshot["bases"] }) {
  if (bases.length === 0) return null;
  const warm = bases.filter((b) => b.state === BaseState.WARM);
  const building = bases.filter((b) => b.state === BaseState.BUILDING);
  const failed = bases.filter((b) => b.state === BaseState.FAILED);
  const topGen = Math.max(...bases.map((b) => b.generation));
  return (
    <span className="font-mono text-[10.5px] text-faint">
      base g{topGen}
      {warm.length > 0 && ` warm on ${warm.map((b) => nodeName(s, b.nodeId)).join(", ")}`}
      {building.length > 0 && <span className="text-primary"> · building {percent(building[0]?.progress ?? 0)}</span>}
      {failed.length > 0 && <span className="text-critical"> · failed on {failed.map((b) => nodeName(s, b.nodeId)).join(", ")}</span>}
    </span>
  );
}

function RepoWorkspaceTable({ s, rows, now }: { s: Snapshot; rows: Workspace[]; now: number }) {
  return (
    <>
      <div className="space-y-2 md:hidden">
        {rows.map((w) => {
          const base = s.bases.find((b) => b.id === w.baseId);
          const live = s.commands.filter((c) => c.workspaceId === w.id && isLive(c)).length;
          return (
            <div
              key={w.id}
              className={`rounded-lg border border-border/70 bg-card/60 p-2.5 font-mono text-[11px] ${w.state === WorkspaceState.RELEASED ? "opacity-50" : ""}`}
            >
              <div className="flex flex-wrap items-baseline justify-between gap-2">
                <div className="min-w-0">
                  <Link to="/workspaces/$id" params={{ id: w.id }} className="font-medium text-foreground hover:text-primary">
                    {w.id}
                  </Link>
                  {w.state !== WorkspaceState.ACTIVE && w.state !== WorkspaceState.IDLE && (
                    <span className={`ml-2 text-[9.5px] ${stateTone[w.state]}`}>{workspaceStateLabel(w.state)}</span>
                  )}
                </div>
                <span className="shrink-0 text-[10.5px]">
                  {live > 0 ? <span className="text-primary">{live} live</span> : <span className="text-faint">{ago(w.lastActiveAt, now)}</span>}
                </span>
              </div>
              <div className="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1 text-[10.5px] text-muted-foreground">
                <SessionLink id={w.sessionId} empty={null} />
                <span>{nodeName(s, w.nodeId)}</span>
                <span className="text-faint">g{base?.generation ?? "?"}</span>
                <span>{w.agent}</span>
              </div>
              <div className="num mt-1.5 flex flex-wrap gap-x-3 gap-y-0.5 text-[10px] text-faint">
                <span>{filesChanged(w)} files</span>
                <span>{bytes(w.exclusiveBytes)} owned</span>
                <span>{String(w.forkMillis)} ms fork</span>
                <span>created {ago(w.createdAt, now)}</span>
              </div>
            </div>
          );
        })}
      </div>

      <div className="hidden md:block">
        <Table className="text-[12px]">
          <TableHeader className="eyebrow">
            <TableRow className="border-border/70 hover:bg-transparent">
              {["workspace", "session", "agent", "node", "base", "files", "owns", "fork", "created", "active"].map((h) => (
                <TableHead key={h} className="h-auto px-2 py-1.5 font-normal text-muted-foreground">
                  {h}
                </TableHead>
              ))}
            </TableRow>
          </TableHeader>
          <TableBody className="num font-mono text-[11px]">
            {rows.map((w) => {
              const base = s.bases.find((b) => b.id === w.baseId);
              const live = s.commands.filter((c) => c.workspaceId === w.id && isLive(c)).length;
              return (
                <TableRow key={w.id} className={`border-border/50 hover:bg-muted/60 ${w.state === WorkspaceState.RELEASED ? "opacity-45" : ""}`}>
                  <TableCell className="px-2 py-2">
                    <Link to="/workspaces/$id" params={{ id: w.id }} className="text-foreground hover:text-primary">
                      {w.id}
                    </Link>
                    {w.state !== WorkspaceState.ACTIVE && w.state !== WorkspaceState.IDLE && <span className={`ml-2 text-[9.5px] ${stateTone[w.state]}`}>{workspaceStateLabel(w.state)}</span>}
                  </TableCell>
                  <TableCell className="px-2 py-2">
                    <SessionLink id={w.sessionId} />
                  </TableCell>
                  <TableCell className="px-2 py-2 text-muted-foreground">{w.agent}</TableCell>
                  <TableCell className="px-2 py-2 text-muted-foreground">{nodeName(s, w.nodeId)}</TableCell>
                  <TableCell className="px-2 py-2 text-faint">g{base?.generation ?? "?"}</TableCell>
                  <TableCell className="px-2 py-2">{filesChanged(w)}</TableCell>
                  <TableCell className="px-2 py-2">{bytes(w.exclusiveBytes)}</TableCell>
                  <TableCell className="px-2 py-2 text-faint">{String(w.forkMillis)} ms</TableCell>
                  <TableCell className="px-2 py-2 text-muted-foreground">{ago(w.createdAt, now)}</TableCell>
                  <TableCell className="px-2 py-2">{live > 0 ? <span className="text-primary">{live} live</span> : <span className="text-faint">{ago(w.lastActiveAt, now)}</span>}</TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      </div>
    </>
  );
}
