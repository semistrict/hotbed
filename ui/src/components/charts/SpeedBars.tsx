import { Link } from "@tanstack/react-router";
import { duration, speedup } from "../../lib/format";
import { CommandInline } from "../CommandBlock";

export interface SpeedRow {
  id: string;
  argv: string;
  sub: string;
  seconds: number;
  coldSeconds: number;
  failed?: boolean;
}

/**
 * Each build as two bars on one scale: the ghost is what it costs from
 * nothing, the ember sliver is what it cost forked from a warm base.
 */
export function SpeedBars({ rows }: { rows: SpeedRow[] }) {
  const max = Math.max(1, ...rows.map((r) => r.coldSeconds));
  return (
    <ul className="space-y-3">
      {rows.map((r, i) => (
        <li key={r.id} className="slide-in" style={{ animationDelay: `${i * 40}ms` }}>
          <Link to="/commands/$id" params={{ id: r.id }} className="group block">
            <div className="mb-1 flex items-baseline gap-3 text-[11.5px]">
              <CommandInline argv={r.argv} className="font-mono text-foreground group-hover:text-primary" />
              <span className="truncate text-faint">{r.sub}</span>
              <span className="ml-auto shrink-0 font-mono">
                <span className="num font-semibold text-foreground">{duration(r.seconds)}</span>
                <span className="text-faint"> vs </span>
                <span className="num text-muted-foreground">{duration(r.coldSeconds)}</span>
              </span>
              <span className="num w-12 shrink-0 text-right font-display text-[15px] font-semibold text-primary">{speedup(r.coldSeconds, r.seconds)}</span>
            </div>
            <div className="relative h-[10px]">
              <div
                className="absolute inset-y-0 left-0 rounded-[3px]"
                style={{
                  width: `${(r.coldSeconds / max) * 100}%`,
                  background: "repeating-linear-gradient(135deg, var(--border-strong) 0 1px, transparent 1px 5px)",
                  boxShadow: "inset 0 0 0 1px var(--border)",
                }}
              />
              <div
                className="absolute inset-y-0 left-0 rounded-[3px]"
                style={{ width: `max(3px, ${(r.seconds / max) * 100}%)`, background: r.failed ? "var(--critical)" : "var(--primary)" }}
              />
            </div>
          </Link>
        </li>
      ))}
    </ul>
  );
}
