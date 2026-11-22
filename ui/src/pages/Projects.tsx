import { Link, useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import type { Project, Repo, Snapshot, Workspace } from "../api";
import { Card, Empty, StatTile } from "../components/ui/Card";
import { nodeName, useSnapshot } from "../lib/data";
import { ago, bytes, compact, duration, percent, quantile } from "../lib/format";
import { isLive } from "../lib/live";
import { useProject } from "../lib/project";
import { remoteName, scope } from "../lib/projects";
import { sessionState } from "../lib/sessions";
import { filterWorkspaces } from "../lib/workspaces";
import { PageTitle } from "./Nodes";
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
  const live = mine.sessions.filter((x) => x.endedAt === null);
  const working = live.filter((x) => sessionState(mine, x) === "working").length;
  const liveWs = mine.workspaces.filter((w) => w.state !== "released");
  return {
    project,
    working,
    idle: live.length - working,
    running: mine.commands.filter(isLive).length,
    finished: mine.history.length,
    failed: mine.history.filter((c) => c.state === "failed").length,
    liveWorkspaces: liveWs.length,
    ownedBytes: liveWs.reduce((n, w) => n + w.exclusiveBytes, 0),
    lastActive: Math.max(0, ...mine.sessions.map((x) => Date.parse(x.lastActiveAt))),
  };
}

// What needs the user first (questions), then what is busy, then what holds workspaces, then most recently touched.
function urgency(a: Activity): number {
  return (a.project.openQuestions > 0 ? 8 : 0) + (a.working > 0 ? 4 : 0) + (a.liveWorkspaces > 0 ? 2 : 0) + (a.idle > 0 ? 1 : 0);
}

function Projects({ s }: { s: Snapshot }) {
  const [chosen, choose] = useProject();
  const [q, setQ] = useState("");
  const now = Date.parse(s.now);
  const visibleProjects = chosen ? s.projects.filter((p) => p.name === chosen) : s.projects;
  const all = visibleProjects
    .map((p) => activity(s, p))
    .sort((a, b) => urgency(b) - urgency(a) || b.lastActive - a.lastActive || a.project.name.localeCompare(b.project.name));

  const scoped = chosen ? scope(s, chosen) : s;
  const liveWs = scoped.workspaces.filter((w) => w.state !== "released");
  const forkMs = liveWs.map((w) => w.forkMillis);
  const owned = liveWs.reduce((n, w) => n + w.exclusiveBytes, 0);
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
        right={
          chosen ? (
            <button
              type="button"
              onClick={() => choose(null)}
              className="rounded-full border border-border px-3 py-1 font-mono text-[11px] text-muted-foreground hover:border-border-strong hover:text-foreground"
            >
              show every project
            </button>
          ) : undefined
        }
      />

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-6">
        <StatTile label="Projects" value={visibleProjects.length} foot={`${workingProjects} working now`} />
        <StatTile label="Repos" value={scoped.repos.length} foot={`${scoped.bases.filter((b) => b.state === "warm").length} warm bases`} />
        <StatTile
          label="Workspaces"
          value={liveWs.length}
          foot={liveWs.length > 0 ? `${bytes(owned)} owned · p50 ${quantile(forkMs, 0.5)} ms fork` : `${compact(s.totals24h.forks)} forks today`}
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
        <div className="rise flex items-center gap-2">
          <input
            type="search"
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder="filter projects, repos, workspaces, sessions or nodes"
            aria-label="Filter projects and workspaces"
            className="w-full rounded-full border border-border bg-background/40 px-3 py-1.5 font-mono text-[11px] text-foreground placeholder:text-faint focus:border-primary focus:outline-none sm:w-96"
          />
          {q && (
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
  const open = (to: "/sessions" | "/commands" | "/questions" | "/reviews") => {
    choose(p.name);
    void navigate({ to });
  };
  const projectMatches = q !== "" && p.name.toLowerCase().includes(q);

  return (
    <Card delay={delay} className={here ? "border-primary/50" : ""}>
      <div className="space-y-4">
        <header className="flex flex-wrap items-baseline gap-x-4 gap-y-2 border-b border-border pb-3">
          <div className="flex items-baseline gap-2.5">
            <button
              type="button"
              onClick={() => choose(here ? null : p.name)}
              title={here ? "Show every project" : `Scope cockpit to ${p.name}`}
              className="display text-left text-[26px] leading-none font-medium hover:text-primary"
            >
              {p.name}
            </button>
            {here && <span className="font-mono text-[10.5px] text-primary">◆ scoped</span>}
            <span className="font-mono text-[11px] text-faint">{a.lastActive > 0 ? `active ${ago(a.lastActive, now)}` : "no sessions today"}</span>
          </div>

          <div className="ml-auto flex flex-wrap items-center gap-3 font-mono text-[11px]">
            <button type="button" onClick={() => open("/sessions")} className="rounded-md border border-border px-2.5 py-1 text-muted-foreground hover:border-border-strong hover:text-foreground">
              <span className={a.working > 0 ? "text-primary" : "text-foreground"}>{a.working + a.idle}</span> sessions
              {a.working > 0 && <span className="text-primary"> · {a.working} working</span>}
            </button>
            <button type="button" onClick={() => open("/commands")} className="rounded-md border border-border px-2.5 py-1 text-muted-foreground hover:border-border-strong hover:text-foreground">
              {a.running > 0 && <span className="text-primary">{a.running} live · </span>}
              <span className="text-foreground">{a.finished}</span> today
              {a.failed > 0 && <span className="text-critical"> · {a.failed} failed</span>}
            </button>
            <button type="button" onClick={() => open("/questions")} className="rounded-md border border-border px-2.5 py-1 text-muted-foreground hover:border-border-strong hover:text-foreground">
              <span className={p.openQuestions > 0 ? "text-primary" : "text-foreground"}>{p.openQuestions}</span> questions
              {p.outbox > 0 && <span className="text-frost"> · {p.outbox} outbox</span>}
            </button>
            <button type="button" onClick={() => open("/reviews")} className="rounded-md border border-border px-2.5 py-1 text-muted-foreground hover:border-border-strong hover:text-foreground">
              <span className={p.activeReviews > 0 ? "text-primary" : "text-foreground"}>{p.activeReviews}</span> reviews
              {p.reviewsToDecide > 0 && <span className="text-primary"> · {p.reviewsToDecide} to decide</span>}
            </button>
          </div>
        </header>

        {p.repos.length === 0 ? (
          <p className="font-mono text-[11.5px] text-faint">No repos in this project yet. Add one with hotbed project add {p.name} --repo REMOTE.</p>
        ) : (
          <div className="space-y-4">
            {p.repos.map((remote) => {
              const repo = s.repos.find((x) => x.remote === remote) ?? null;
              const repoMatches = projectMatches || remote.toLowerCase().includes(q) || (repo?.id.toLowerCase().includes(q) ?? false);
              return (
                <RepoBlock
                  key={remote}
                  s={s}
                  remote={remote}
                  repo={repo}
                  now={now}
                  q={repoMatches ? "" : q}
                />
              );
            })}
          </div>
        )}
      </div>
    </Card>
  );
}

function RepoBlock({ s, remote, repo, now, q }: { s: Snapshot; remote: string; repo: Repo | null; now: number; q: string }) {
  const [showReleased, setShowReleased] = useState(false);
  const bases = repo ? s.bases.filter((b) => b.repoId === repo.id && b.state !== "stale") : [];
  const allWorkspaces = repo ? filterWorkspaces(s.workspaces, { repo: repo.id, q }, (id) => nodeName(s, id)) : [];
  const live = allWorkspaces.filter((w) => w.state !== "released");
  const released = allWorkspaces.filter((w) => w.state === "released");
  const owned = live.reduce((n, w) => n + w.exclusiveBytes, 0);
  const shown = showReleased || q !== "" ? allWorkspaces : live;

  // When searching workspaces and this repo has none matching, stay compact.
  if (q !== "" && allWorkspaces.length === 0) return null;

  return (
    <div className="rounded-xl border border-border/80 bg-background/35 p-4">
      <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
        <span className="font-mono text-[13px] font-medium text-foreground" title={remote}>
          {remoteName(remote)}
        </span>
        {repo ? (
          <>
            <span className="font-mono text-[11px] text-muted-foreground">
              {repo.language}
              {repo.units > 0 && ` · ${repo.units} ${repo.language === "rust" ? "crates" : "packages"}`}
              {repo.coldBuildSeconds > 0 && ` · cold ${duration(repo.coldBuildSeconds)}`}
            </span>
            <BasesSummary s={s} bases={bases} />
          </>
        ) : (
          <span className="font-mono text-[11px] text-faint">not kept by hotbed</span>
        )}
        <span className="ml-auto font-mono text-[11px] text-muted-foreground">
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

function BasesSummary({ s, bases }: { s: Snapshot; bases: Snapshot["bases"] }) {
  if (bases.length === 0) return null;
  const warm = bases.filter((b) => b.state === "warm");
  const building = bases.filter((b) => b.state === "building");
  const topGen = Math.max(...bases.map((b) => b.generation));
  return (
    <span className="font-mono text-[10.5px] text-faint">
      base g{topGen}
      {warm.length > 0 && ` warm on ${warm.map((b) => nodeName(s, b.nodeId)).join(", ")}`}
      {building.length > 0 && <span className="text-primary"> · building {percent(building[0]?.progress ?? 0)}</span>}
    </span>
  );
}

function RepoWorkspaceTable({ s, rows, now }: { s: Snapshot; rows: Workspace[]; now: number }) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-[12px]">
        <thead className="eyebrow">
          <tr className="border-b border-border/70">
            {["workspace", "session", "agent", "node", "base", "files", "owns", "fork", "created", "active"].map((h) => (
              <th key={h} className="px-2 py-1.5 font-normal whitespace-nowrap">
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="num font-mono text-[11px] [&_td]:whitespace-nowrap">
          {rows.map((w) => {
            const base = s.bases.find((b) => b.id === w.baseId);
            const live = s.commands.filter((c) => c.workspaceId === w.id && isLive(c)).length;
            return (
              <tr key={w.id} className={`border-b border-border/50 last:border-b-0 hover:bg-muted/60 ${w.state === "released" ? "opacity-45" : ""}`}>
                <td className="px-2 py-2">
                  <Link to="/workspaces/$id" params={{ id: w.id }} className="text-foreground hover:text-primary">
                    {w.id}
                  </Link>
                  {w.state !== "active" && w.state !== "idle" && <span className={`ml-2 text-[9.5px] ${stateTone[w.state]}`}>{w.state}</span>}
                </td>
                <td className="px-2 py-2">
                  {w.sessionId ? (
                    <Link to="/sessions/$id" params={{ id: w.sessionId }} className="text-muted-foreground hover:text-primary">
                      {w.sessionId}
                    </Link>
                  ) : (
                    <span className="text-faint">—</span>
                  )}
                </td>
                <td className="px-2 py-2 text-muted-foreground">{w.agent}</td>
                <td className="px-2 py-2 text-muted-foreground">{nodeName(s, w.nodeId)}</td>
                <td className="px-2 py-2 text-faint">g{base?.generation ?? "?"}</td>
                <td className="px-2 py-2">{w.filesChanged}</td>
                <td className="px-2 py-2">{bytes(w.exclusiveBytes)}</td>
                <td className="px-2 py-2 text-faint">{w.forkMillis} ms</td>
                <td className="px-2 py-2 text-muted-foreground">{ago(w.createdAt, now)}</td>
                <td className="px-2 py-2">{live > 0 ? <span className="text-primary">{live} live</span> : <span className="text-faint">{ago(w.lastActiveAt, now)}</span>}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
