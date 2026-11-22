import { Link, Outlet } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { usingMock } from "../api";
import { useSnapshot } from "../lib/data";

const NAV = [
  { to: "/", label: "Sessions" },
  { to: "/schedules", label: "Schedules" },
  { to: "/cluster", label: "Cluster" },
  { to: "/nodes", label: "Nodes" },
  { to: "/workspaces", label: "Workspaces" },
  { to: "/commands", label: "Commands" },
  { to: "/history", label: "History" },
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
  const { data, isError, error } = useSnapshot();
  const [theme, nextTheme] = useTheme();
  const online = data?.nodes.filter((n) => n.online).length ?? 0;

  return (
    <div className="mx-auto max-w-[1440px] px-4 pb-16 sm:px-6 lg:px-8">
      <header className="sticky top-0 z-40 -mx-4 mb-6 border-b border-border bg-background/75 px-4 backdrop-blur-md sm:-mx-6 sm:px-6 lg:-mx-8 lg:px-8">
        <div className="flex h-14 items-center gap-6">
          <Link to="/" className="flex items-center gap-2.5">
            <Logo />
            <span className="display text-[22px] leading-none font-semibold italic">hotbed</span>
          </Link>
          <nav className="-mb-px flex h-full items-stretch gap-1 overflow-x-auto">
            {NAV.map((n) => (
              <Link
                key={n.to}
                to={n.to}
                activeOptions={{ exact: n.to === "/" }}
                className="flex items-center border-b-2 border-transparent px-2.5 text-[12.5px] text-muted-foreground transition-colors hover:text-foreground"
                activeProps={{ className: "!border-primary !text-foreground" }}
              >
                {n.label}
              </Link>
            ))}
          </nav>
          <div className="ml-auto flex items-center gap-4 font-mono text-[10.5px] text-muted-foreground">
            {usingMock && <span className="hidden rounded border border-dashed border-border-strong px-1.5 py-0.5 text-faint md:inline">simulated cluster</span>}
            {data && (
              <span className="hidden items-center gap-2 sm:flex">
                <span className="live-dot inline-block size-1.5 rounded-full bg-primary text-primary" />
                <span>
                  {data.controller} · {online}/{data.nodes.length} nodes
                </span>
              </span>
            )}
            <button type="button" onClick={nextTheme} className="rounded-md border border-border px-2 py-1 whitespace-nowrap hover:border-border-strong hover:text-foreground" title="Theme">
              {theme === "system" ? "◐ auto" : theme === "dark" ? "● dark" : "○ light"}
            </button>
          </div>
        </div>
      </header>
      {isError ? (
        <div className="rounded-xl border border-critical/40 bg-card p-6 font-mono text-[12px] text-critical">Cannot reach the controller: {String(error)}</div>
      ) : data ? (
        <Outlet />
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
