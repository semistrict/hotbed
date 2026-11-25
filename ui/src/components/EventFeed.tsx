import { Link } from "@tanstack/react-router";
import type { ClusterEvent } from "../api";
import { hhmmss } from "../lib/format";

const KIND: Record<ClusterEvent["kind"], { glyph: string; className: string }> = {
  fork: { glyph: "⑂", className: "text-sprout" },
  release: { glyph: "↺", className: "text-faint" },
  admit: { glyph: "▸", className: "text-muted-foreground" },
  wait: { glyph: "…", className: "text-warning-ink" },
  hold: { glyph: "‖", className: "text-serious-ink" },
  unhold: { glyph: "▶", className: "text-good-ink" },
  pause: { glyph: "⏸", className: "text-frost" },
  resume: { glyph: "↻", className: "text-primary" },
  finish: { glyph: "✓", className: "text-good-ink" },
  fail: { glyph: "✕", className: "text-critical" },
  park: { glyph: "❄", className: "text-frost" },
  thaw: { glyph: "☀", className: "text-primary" },
  base: { glyph: "▣", className: "text-primary" },
  node: { glyph: "◆", className: "text-muted-foreground" },
};

/** The cluster's log, newest first; each line links to what it is about. With hosts, each says its node. */
export function EventFeed({ events, limit = 18, hosts }: { events: ClusterEvent[]; limit?: number; hosts?: Map<string, string> }) {
  if (events.length === 0) return <p className="py-6 text-center text-[12px] text-muted-foreground">Nothing yet.</p>;
  return (
    <ol className="space-y-px font-mono text-[11px] leading-[1.7]">
      {events.slice(0, limit).map((e) => {
        const k = KIND[e.kind] ?? KIND.node!;
        const body = (
          <>
            <span className="num shrink-0 text-faint">{hhmmss(e.at)}</span>
            <span className={`w-3 shrink-0 text-center ${k.className}`} aria-hidden>
              {k.glyph}
            </span>
            {hosts && <span className="w-20 shrink-0 truncate text-faint">{hosts.get(e.nodeId) ?? ""}</span>}
            <span className="truncate text-muted-foreground group-hover:text-foreground" title={e.text}>
              {e.text}
            </span>
          </>
        );
        const baseCls = "flex gap-2.5 rounded px-1.5";
        const linkCls = `group ${baseCls} hover:bg-muted`;
        return (
          <li key={String(e.id)} className="slide-in">
            {e.ref?.type === "command" ? (
              <Link to="/commands/$id" params={{ id: e.ref.id }} className={linkCls}>
                {body}
              </Link>
            ) : e.ref?.type === "workspace" ? (
              <Link to="/workspaces/$id" params={{ id: e.ref.id }} className={linkCls}>
                {body}
              </Link>
            ) : (
              <div className={baseCls}>{body}</div>
            )}
          </li>
        );
      })}
    </ol>
  );
}
