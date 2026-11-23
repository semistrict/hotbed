import { Link, Outlet } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import type { Snapshot } from "../api";
import { useSnapshot } from "../lib/data";
import { useProject, useScopedSnapshot } from "../lib/project";

// A project's work first, as the chosen project sees it; then the machines,
// which every project shares.
const NAV = [
  { to: "/", label: "Projects" },
  { to: "/questions", label: "Questions" },
  { to: "/reviews", label: "Reviews" },
  { to: "/sessions", label: "Sessions" },
  { to: "/commands", label: "Commands" },
  { to: "/schedules", label: "Schedules" },
  { to: "/cluster", label: "Cluster", shared: true },
  { to: "/nodes", label: "Nodes", shared: true },
  { to: "/logs", label: "Logs", shared: true },
] as const;

type Theme = "system" | "light" | "dark";

function useTheme(): [Theme, () => void] {
  const [theme, setTheme] = useState<Theme>(() => {
    try {
      return (localStorage.getItem("hotbed-theme") as Theme | null) ?? "system";
    } catch {
      return "system";
    }
  });
  useEffect(() => {
    const root = document.documentElement;
    if (theme === "system") root.removeAttribute("data-theme");
    else root.setAttribute("data-theme", theme);
    try {
      localStorage.setItem("hotbed-theme", theme);
    } catch {
      // Private windows refuse storage; the theme still applies for this visit.
    }
  }, [theme]);
  const next = () => setTheme((t) => (t === "system" ? "dark" : t === "dark" ? "light" : "system"));
  return [theme, next];
}

export function Shell() {
  const { data: whole, isError, error } = useSnapshot();
  const { data } = useScopedSnapshot();
  const [theme, nextTheme] = useTheme();
  const online = whole?.nodes.filter((n) => n.online).length ?? 0;

  return (
    <div className="mx-auto max-w-[1440px] px-4 pb-16 sm:px-6 lg:px-8">
      <header className="sticky top-0 z-40 -mx-4 mb-6 border-b border-border bg-background/75 px-4 backdrop-blur-md sm:-mx-6 sm:px-6 lg:-mx-8 lg:px-8">
        <div className="flex h-14 items-center gap-6">
          <Link to="/" className="flex items-center gap-2.5">
            <Logo />
            <span className="display text-[22px] leading-none font-semibold italic">hotbed</span>
          </Link>
          <nav className="-mb-px flex h-full items-stretch gap-1 overflow-x-auto">
            <ProjectPicker />
            {NAV.map((n) => (
              <Link
                key={n.to}
                to={n.to}
                title={"shared" in n ? "The machines, which every project shares" : undefined}
                activeOptions={{ exact: n.to === "/" }}
                className={`flex items-center border-b-2 border-transparent px-2.5 text-[12.5px] whitespace-nowrap text-muted-foreground transition-colors hover:text-foreground ${n.to === "/cluster" ? "ml-3 border-l border-l-border pl-4" : ""}`}
                activeProps={{ className: "!border-primary !text-foreground" }}
              >
                {n.label}
                {n.to === "/reviews" && data && <ReviewBadge s={data} />}
                {n.to === "/questions" && data && data.openQuestions > 0 && (
                  <span className="ml-1.5 rounded-full bg-primary px-1.5 font-mono text-[10px] leading-4 text-primary-foreground" aria-label={`${data.openQuestions} waiting for you`}>
                    {data.openQuestions}
                  </span>
                )}
                {n.to === "/logs" && whole && <DaemonLogsBadge s={whole} />}
              </Link>
            ))}
          </nav>
          <div className="ml-auto flex items-center gap-4 font-mono text-[10.5px] text-muted-foreground">
            {whole && (
              <span className="hidden items-center gap-2 whitespace-nowrap 2xl:flex">
                <span className="live-dot inline-block size-1.5 rounded-full bg-primary text-primary" />
                <span>
                  {whole.controller} · {online}/{whole.nodes.length} nodes
                </span>
              </span>
            )}
            <button type="button" onClick={nextTheme} className="rounded-md border border-border px-2 py-1 whitespace-nowrap hover:border-border-strong hover:text-foreground" title="Theme">
              {theme === "system" ? "◐ auto" : theme === "dark" ? "● dark" : "○ light"}
            </button>
          </div>
        </div>
      </header>
      {whole ? (
        <>
          {isError && (
            <div className="mb-4 rounded-xl border border-critical/40 bg-card px-4 py-2.5 font-mono text-[11.5px] text-critical">
              Lost connection to the controller — showing last snapshot ({String(error)})
            </div>
          )}
          <Outlet />
        </>
      ) : isError ? (
        <div className="rounded-xl border border-critical/40 bg-card p-6 font-mono text-[12px] text-critical">Cannot reach the controller: {String(error)}</div>
      ) : (
        <div className="py-24 text-center font-mono text-[12px] text-muted-foreground">warming up…</div>
      )}
    </div>
  );
}

function Logo() {
  return (
    <svg width="26" height="26" viewBox="0 0 32 32" aria-hidden>
      <path d="M16 25c0-6 0-9 4.5-12.5" stroke="var(--sprout)" strokeWidth="2.4" strokeLinecap="round" fill="none" />
      <path d="M16 25c0-4-1.5-6.5-5.5-8.5" stroke="var(--sprout)" strokeWidth="2.4" strokeLinecap="round" fill="none" />
      <path d="M20.5 12.5c1.8-1.6 4.2-2 5.5-1.6-.3 1.6-1.8 3.4-4.4 3.9" fill="var(--sprout)" />
      <path d="M5 26h22" stroke="var(--primary)" strokeWidth="2.6" strokeLinecap="round" />
      <path d="M8 29.5h16" stroke="var(--primary)" strokeOpacity=".45" strokeWidth="1.6" strokeLinecap="round" />
    </svg>
  );
}

/** Which project the cockpit shows: one, or every one. */
function ProjectPicker() {
  const { data } = useSnapshot();
  const [project, choose] = useProject();
  if (!data) return null;
  return (
    <label className="mr-2 flex items-center gap-1.5 self-center rounded-lg border border-border bg-card/70 py-1 pr-1 pl-2.5 font-mono text-[11px] text-muted-foreground focus-within:border-primary/50">
      <span aria-hidden className={project ? "text-primary" : "text-faint"}>
        ◆
      </span>
      <select
        aria-label="Project"
        value={project ?? ""}
        onChange={(e) => choose(e.target.value || null)}
        className="max-w-40 bg-transparent py-0.5 text-[12px] text-foreground focus:outline-none"
      >
        <option value="">every project</option>
        {data.projects.map((p) => (
          <option key={p.name} value={p.name}>
            {p.name}
            {p.openQuestions > 0 ? ` (${p.openQuestions} ?)` : ""}
          </option>
        ))}
      </select>
    </label>
  );
}

/** Reviews of the project shown (or every project) done with findings still undecided. */
function ReviewBadge({ s }: { s: Snapshot }) {
  const [project] = useProject();
  const n = s.projects.filter((p) => project === null || p.name === project).reduce((sum, p) => sum + p.reviewsToDecide, 0);
  if (n === 0) return null;
  return (
    <span className="ml-1.5 rounded-full border border-primary/50 px-1.5 font-mono text-[10px] leading-4 text-primary" aria-label={`${n} with findings to decide`}>
      {n}
    </span>
  );
}

/** 24h WARN/ERROR slog counts across the controller and every worker. */
function DaemonLogsBadge({ s }: { s: Snapshot }) {
  const errors = (s.controllerLogs?.errors24h ?? 0) + s.nodes.reduce((sum, n) => sum + (n.daemonLogs?.errors24h ?? 0), 0);
  const warns = (s.controllerLogs?.warns24h ?? 0) + s.nodes.reduce((sum, n) => sum + (n.daemonLogs?.warns24h ?? 0), 0);
  if (errors > 0) {
    return (
      <span
        className="ml-1.5 rounded-full bg-[color-mix(in_oklab,var(--critical)_18%,transparent)] px-1.5 font-mono text-[10px] leading-4 text-critical"
        title={`${errors} errors, ${warns} warnings in the last 24h`}
      >
        {errors}
      </span>
    );
  }
  if (warns > 0) {
    return (
      <span
        className="ml-1.5 rounded-full bg-[color-mix(in_oklab,var(--warning)_22%,transparent)] px-1.5 font-mono text-[10px] leading-4 text-warning"
        title={`${warns} warnings in the last 24h`}
      >
        {warns}
      </span>
    );
  }
  return null;
}
