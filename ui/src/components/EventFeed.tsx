import { Link } from "@tanstack/react-router";
import type { ClusterEvent } from "../api";
import { hhmmss } from "../lib/format";

const KIND: Record<ClusterEvent["kind"], { glyph: string; className: string }> = {
  fork: { glyph: "⑂", className: "text-sprout" },
  release: { glyph: "↺", className: "text-faint" },
  admit: { glyph: "▸", className: "text-muted-foreground" },
  finish: { glyph: "✓", className: "text-good-ink" },
  fail: { glyph: "✕", className: "text-critical" },
  park: { glyph: "❄", className: "text-frost" },
  thaw: { glyph: "☀", className: "text-primary" },
  base: { glyph: "▣", className: "text-primary" },
  node: { glyph: "◆", className: "text-muted-foreground" },
};

/** The cluster's log, newest first; each line links to what it is about. */
export function EventFeed({ events, limit = 18 }: { events: ClusterEvent[]; limit?: number }) {
  return (
    <ol className="space-y-px font-mono text-[11px] leading-[1.7]">
      {events.slice(0, limit).map((e) => {
        const k = KIND[e.kind];
        const body = (
          <>
            <span className="num shrink-0 text-faint">{hhmmss(Date.parse(e.at))}</span>
            <span className={`w-3 shrink-0 text-center ${k.className}`} aria-hidden>
              {k.glyph}
            </span>
            <span className="truncate text-muted-foreground group-hover:text-foreground">{e.text}</span>
          </>
        );
        const cls = "group flex gap-2.5 rounded px-1.5 hover:bg-muted";
        return (
          <li key={e.id} className="slide-in">
            {e.ref?.type === "command" ? (
              <Link to="/commands/$id" params={{ id: e.ref.id }} className={cls}>
                {body}
              </Link>
            ) : e.ref?.type === "workspace" ? (
              <Link to="/workspaces/$id" params={{ id: e.ref.id }} className={cls}>
                {body}
              </Link>
            ) : (
              <div className={cls}>{body}</div>
            )}
          </li>
        );
      })}
    </ol>
  );
}
