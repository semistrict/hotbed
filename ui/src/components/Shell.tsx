import { Link, Outlet, useNavigate } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Select, SelectContent, SelectItem, SelectSeparator, SelectTrigger, SelectValue } from "@/components/ui/select";
import type { Snapshot } from "../api";
import { signOut, useAuthMe, useSwitchOrganization } from "../lib/auth";
import { useSnapshot } from "../lib/data";
import { useProject, useScopedSnapshot } from "../lib/project";

// A project's work first, as the chosen project sees it; then the machines,
// which every project shares.
const NAV = [
  { to: "/", label: "Projects" },
  { to: "/questions", label: "Questions" },
  { to: "/memories", label: "Memories" },
  { to: "/reviews", label: "Reviews" },
  { to: "/sessions", label: "Sessions" },
  { to: "/commands", label: "Commands" },
  { to: "/schedules", label: "Schedules" },
  { to: "/cluster", label: "Cluster", shared: true },
  { to: "/nodes", label: "Nodes", shared: true },
  { to: "/logs", label: "Logs", shared: true },
  { to: "/org", label: "Org", shared: true },
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
            <OrgPicker />
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
                  <Badge className="ml-1.5 h-4 min-w-4 px-1.5 font-mono text-[10px] leading-4" aria-label={`${data.openQuestions} waiting for you`}>
                    {data.openQuestions}
                  </Badge>
                )}
                {n.to === "/memories" && data && data.pendingMemories > 0 && (
                  <Badge className="ml-1.5 h-4 min-w-4 px-1.5 font-mono text-[10px] leading-4" aria-label={`${data.pendingMemories} pending proposals`}>
                    {data.pendingMemories}
                  </Badge>
                )}
                {n.to === "/logs" && whole && <DaemonLogsBadge s={whole} />}
              </Link>
            ))}
          </nav>
          <div className="ml-auto flex items-center gap-3 font-mono text-[10.5px] text-muted-foreground">
            {whole && (
              <span className="hidden items-center gap-2 whitespace-nowrap 2xl:flex">
                <span className="live-dot inline-block size-1.5 rounded-full bg-primary text-primary" />
                <span>
                  {whole.controller} · {online}/{whole.nodes.length} nodes
                </span>
              </span>
            )}
            <AccountControls />
            <Button type="button" variant="outline" size="xs" onClick={nextTheme} className="font-mono text-[10.5px] text-muted-foreground hover:text-foreground" title="Theme">
              {theme === "system" ? "◐ auto" : theme === "dark" ? "● dark" : "○ light"}
            </Button>
          </div>
        </div>
      </header>
      {whole ? (
        <>
          {isError && (
            <Alert variant="destructive" className="mb-4 rounded-xl border-critical/40 bg-card px-4 py-2.5 font-mono text-[11.5px] text-critical">
              <AlertDescription className="text-[11.5px] text-critical">
                Lost connection to the controller — showing last snapshot ({String(error)})
              </AlertDescription>
            </Alert>
          )}
          <Outlet />
        </>
      ) : isError ? (
        <Alert variant="destructive" className="rounded-xl border-critical/40 bg-card p-6 font-mono text-[12px] text-critical">
          <AlertDescription className="text-[12px] text-critical">Cannot reach the controller: {String(error)}</AlertDescription>
        </Alert>
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

/** Which WorkOS organization the cockpit targets; switching moves to that org's ControllerDO. */
function OrgPicker() {
  const { data: auth } = useAuthMe();
  const switchOrg = useSwitchOrganization();
  const navigate = useNavigate();
  const [, chooseProject] = useProject();
  if (!auth?.workosEnabled || !auth.organization) return null;
  const orgs = auth.organizations.length > 0 ? auth.organizations : [auth.organization];
  const items = [
    ...orgs.map((o) => ({ value: o.id, label: o.name })),
    { value: "__manage__", label: "+ Manage organizations…" },
  ];

  return (
    <div className="mr-1 flex items-center self-center">
      <Select
        items={items}
        value={auth.organization.id}
        disabled={switchOrg.isPending}
        onValueChange={(val) => {
          if (!val) return;
          if (val === "__manage__") {
            void navigate({ to: "/org" });
            return;
          }
          if (val !== auth.organization?.id) {
            chooseProject(null);
            switchOrg.mutate(val);
          }
        }}
      >
        <SelectTrigger
          size="sm"
          aria-label="Organization"
          className="h-7 max-w-44 gap-1.5 border-border bg-card/70 px-2.5 font-mono text-[11.5px] text-foreground hover:border-border-strong"
        >
          <span aria-hidden className="text-primary">
            ◈
          </span>
          <SelectValue />
        </SelectTrigger>
        <SelectContent className="min-w-48 font-mono text-[12px]">
          {orgs.map((o) => (
            <SelectItem key={o.id} value={o.id} className="text-[12px]">
              {o.name}
            </SelectItem>
          ))}
          <SelectSeparator />
          <SelectItem value="__manage__" className="text-[12px] text-muted-foreground">
            + Manage organizations…
          </SelectItem>
        </SelectContent>
      </Select>
    </div>
  );
}

function AccountControls() {
  const { data: auth } = useAuthMe();
  if (!auth?.authenticated || !auth.workosEnabled) return null;
  return (
    <span className="flex items-center gap-2 whitespace-nowrap">
      <Link
        to="/org"
        className="hidden max-w-40 truncate text-muted-foreground hover:text-foreground xl:inline"
        title={`${auth.user.name} (${auth.user.email})`}
      >
        {auth.user.email}
      </Link>
      <Button
        type="button"
        variant="outline"
        size="xs"
        onClick={() => void signOut()}
        className="font-mono text-[10.5px] text-muted-foreground hover:text-foreground"
        title="Sign out"
      >
        sign out
      </Button>
    </span>
  );
}

const ALL_PROJECTS = "__all__";

/** Which project the cockpit shows: one, or every one. */
function ProjectPicker() {
  const { data } = useSnapshot();
  const [project, choose] = useProject();
  if (!data) return null;
  const items = [
    { value: ALL_PROJECTS, label: "every project" },
    ...data.projects.map((p) => ({
      value: p.name,
      label: p.openQuestions > 0 ? `${p.name} (${p.openQuestions} ?)` : p.name,
    })),
  ];

  return (
    <div className="mr-2 flex items-center self-center">
      <Select
        items={items}
        value={project ?? ALL_PROJECTS}
        onValueChange={(val) => choose(!val || val === ALL_PROJECTS ? null : val)}
      >
        <SelectTrigger
          size="sm"
          aria-label="Project"
          className="h-7 max-w-44 gap-1.5 border-border bg-card/70 px-2.5 font-mono text-[11.5px] text-foreground hover:border-border-strong"
        >
          <span aria-hidden className={project ? "text-primary" : "text-faint"}>
            ◆
          </span>
          <SelectValue />
        </SelectTrigger>
        <SelectContent className="min-w-44 font-mono text-[12px]">
          <SelectItem value={ALL_PROJECTS} className="text-[12px]">
            every project
          </SelectItem>
          {data.projects.length > 0 && <SelectSeparator />}
          {data.projects.map((p) => (
            <SelectItem key={p.name} value={p.name} className="text-[12px]">
              {p.name}
              {p.openQuestions > 0 ? ` (${p.openQuestions} ?)` : ""}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  );
}

/** Reviews of the project shown (or every project) done with findings still undecided. */
function ReviewBadge({ s }: { s: Snapshot }) {
  const [project] = useProject();
  const n = s.projects.filter((p) => project === null || p.name === project).reduce((sum, p) => sum + p.reviewsToDecide, 0);
  if (n === 0) return null;
  return (
    <Badge variant="outline" className="ml-1.5 h-4 min-w-4 border-primary/50 px-1.5 font-mono text-[10px] leading-4 text-primary" aria-label={`${n} with findings to decide`}>
      {n}
    </Badge>
  );
}

/** 24h WARN/ERROR slog counts across the controller and every worker. */
function DaemonLogsBadge({ s }: { s: Snapshot }) {
  const errors = (s.controllerLogs?.errors24h ?? 0) + s.nodes.reduce((sum, n) => sum + (n.daemonLogs?.errors24h ?? 0), 0);
  const warns = (s.controllerLogs?.warns24h ?? 0) + s.nodes.reduce((sum, n) => sum + (n.daemonLogs?.warns24h ?? 0), 0);
  if (errors > 0) {
    return (
      <Badge
        variant="destructive"
        className="ml-1.5 h-4 min-w-4 bg-[color-mix(in_oklab,var(--critical)_18%,transparent)] px-1.5 font-mono text-[10px] leading-4 text-critical"
        title={`${errors} errors, ${warns} warnings in the last 24h`}
      >
        {errors}
      </Badge>
    );
  }
  if (warns > 0) {
    return (
      <Badge
        variant="secondary"
        className="ml-1.5 h-4 min-w-4 bg-[color-mix(in_oklab,var(--warning)_22%,transparent)] px-1.5 font-mono text-[10px] leading-4 text-warning"
        title={`${warns} warnings in the last 24h`}
      >
        {warns}
      </Badge>
    );
  }
  return null;
}
