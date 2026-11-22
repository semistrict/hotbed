import { useNavigate } from "@tanstack/react-router";
import type { Project, Snapshot } from "../api";
import { Card, Empty, StatTile } from "../components/ui/Card";
import { useSnapshot } from "../lib/data";
import { ago } from "../lib/format";
import { isLive } from "../lib/live";
import { useProject } from "../lib/project";
import { remoteName, scope } from "../lib/projects";
import { sessionState } from "../lib/sessions";
import { PageTitle } from "./Nodes";

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
  /** When one of its sessions was last touched; 0 for never. */
  lastActive: number;
}

function activity(s: Snapshot, project: Project): Activity {
  const mine = scope(s, project.name);
  const live = mine.sessions.filter((x) => x.endedAt === null);
  const working = live.filter((x) => sessionState(mine, x) === "working").length;
  return {
    project,
    working,
    idle: live.length - working,
    running: mine.commands.filter(isLive).length,
    finished: mine.history.length,
    failed: mine.history.filter((c) => c.state === "failed").length,
    lastActive: Math.max(0, ...mine.sessions.map((x) => Date.parse(x.lastActiveAt))),
  };
}

// What needs the user first (questions), then what is busy, then the most recently touched.
function urgency(a: Activity): number {
  return (a.project.openQuestions > 0 ? 4 : 0) + (a.working > 0 ? 2 : 0) + (a.idle > 0 ? 1 : 0);
}

function Projects({ s }: { s: Snapshot }) {
  const now = Date.parse(s.now);
  const all = s.projects.map((p) => activity(s, p)).sort((a, b) => urgency(b) - urgency(a) || b.lastActive - a.lastActive || a.project.name.localeCompare(b.project.name));
  const live = all.reduce((n, a) => n + a.working + a.idle, 0);
  const outbox = s.projects.reduce((n, p) => n + p.outbox, 0);
  const busy = all.filter((a) => a.working > 0).length;

  return (
    <div className="space-y-6">
      <PageTitle
        title="Projects"
        sub="What sessions work on and questions are about: each one or more repos, named by their git remotes. Choose one to see sessions, questions, commands and history as it sees them."
      />
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <StatTile label="Projects" value={s.projects.length} foot={`${busy} working now`} />
        <StatTile label="Live sessions" value={live} tone="ember" foot="across every project" />
        <StatTile label="Questions for you" value={s.openQuestions} tone={s.openQuestions > 0 ? "ember" : "default"} foot={`${outbox} answered, waiting for agents`} />
        <StatTile label="Commands today" value={all.reduce((n, a) => n + a.finished, 0)} foot={`${all.reduce((n, a) => n + a.failed, 0)} failed`} />
      </div>
      {all.length === 0 ? (
        <Card>
          <Empty>No projects yet. A session makes one for its checkout's repo; hotbed project add NAME --repo REMOTE makes one of several repos.</Empty>
        </Card>
      ) : (
        <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
          {all.map((a, i) => (
            <ProjectCard key={a.project.name} s={s} a={a} now={now} delay={40 * Math.min(i, 8)} />
          ))}
        </div>
      )}
    </div>
  );
}

function ProjectCard({ s, a, now, delay }: { s: Snapshot; a: Activity; now: number; delay: number }) {
  const [chosen, choose] = useProject();
  const navigate = useNavigate();
  const p = a.project;
  const open = (to: "/sessions" | "/questions" | "/reviews") => {
    choose(p.name);
    void navigate({ to });
  };
  const here = chosen === p.name;
  return (
    <Card delay={delay} className={here ? "border-primary/50" : ""}>
      <div className="space-y-4">
        <header className="flex items-baseline gap-2">
          <button type="button" onClick={() => open("/sessions")} className="display text-left text-[24px] leading-none font-medium hover:text-primary">
            {p.name}
          </button>
          {here && <span className="font-mono text-[10.5px] text-primary">◆ shown</span>}
          <span className="ml-auto font-mono text-[10.5px] text-faint">{a.lastActive > 0 ? `active ${ago(a.lastActive, now)}` : "no sessions today"}</span>
        </header>
        <ul className="space-y-1 font-mono text-[11px]">
          {p.repos.length === 0 && <li className="text-faint">no repos</li>}
          {p.repos.map((r) => {
            const repo = s.repos.find((x) => x.remote === r);
            return (
              <li key={r} className="flex items-baseline gap-2" title={r}>
                <span className="text-foreground">{remoteName(r)}</span>
                {repo ? (
                  <span className="text-faint">
                    {repo.language} · {repo.units > 0 ? `${repo.units} units` : "built on demand"}
                  </span>
                ) : (
                  <span className="text-faint">not kept by hotbed</span>
                )}
              </li>
            );
          })}
        </ul>
        <dl className="grid grid-cols-2 gap-2 border-t border-border pt-3 text-[11px] sm:grid-cols-4">
          <Figure label="sessions" value={a.working + a.idle} note={a.working > 0 ? `${a.working} working` : a.idle > 0 ? "idle" : ""} tone={a.working > 0 ? "text-primary" : ""} />
          <Figure label="running" value={a.running} note={a.failed > 0 ? `${a.failed} of ${a.finished} failed today` : `${a.finished} today`} tone="text-primary" noteTone={a.failed > 0 ? "text-critical" : ""} />
          <Figure label="questions" value={p.openQuestions} note={p.outbox > 0 ? `${p.outbox} in the outbox` : "for you"} tone={p.openQuestions > 0 ? "text-primary" : ""} onClick={() => open("/questions")} />
          <Figure label="reviews" value={p.activeReviews} note={p.reviewsToDecide > 0 ? `${p.reviewsToDecide} to decide` : "under way"} tone="text-primary" noteTone={p.reviewsToDecide > 0 ? "text-primary" : ""} onClick={() => open("/reviews")} />
        </dl>
      </div>
    </Card>
  );
}

function Figure({ label, value, note, tone = "", noteTone = "", onClick }: { label: string; value: number; note: string; tone?: string; noteTone?: string; onClick?: () => void }) {
  const figure = <span className={`display mt-1 block text-[24px] leading-none font-medium ${value === 0 ? "text-faint" : tone}`}>{value}</span>;
  return (
    <div>
      <dt className="eyebrow">{label}</dt>
      <dd>
        {onClick ? (
          <button type="button" onClick={onClick} className="hover:[&>span]:text-primary" title={`Show ${label}`}>
            {figure}
          </button>
        ) : (
          figure
        )}
        <span className={`mt-1 block text-[10.5px] text-muted-foreground ${noteTone}`}>{note}</span>
      </dd>
    </div>
  );
}
