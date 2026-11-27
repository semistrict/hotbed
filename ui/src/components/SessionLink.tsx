import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";
import { HoverCard, HoverCardContent, HoverCardTrigger } from "@/components/ui/hover-card";
import { cn } from "@/lib/utils";
import type { Session, Snapshot } from "../api";
import { useSession } from "../lib/data";
import { ago } from "../lib/format";
import { sessionState, type SessionState } from "../lib/sessions";
import { ProjectTag } from "./ProjectTag";
import { T3Mark } from "./T3Mark";

/**
 * A session's name as a badge opening its page, wherever the cockpit names a
 * session. Hovering it shows the session (agent, model, project, when) and
 * opens its T3 Code thread, when it has one. The card asks for the session
 * only once shown, so a long list of badges costs nothing. With no session
 * (an empty id) it shows `empty`: a dash, for a table cell, unless told.
 */
export function SessionLink({ id, className = "", empty = <span className="font-mono text-[11px] text-faint">—</span> }: { id: string; className?: string; empty?: ReactNode }) {
  if (!id) return empty;
  return (
    <HoverCard>
      <HoverCardTrigger
        delay={300}
        render={
          <Link
            to="/sessions/$id"
            params={{ id }}
            className={cn("inline-flex items-center rounded border border-border/70 px-1 font-mono text-[11px] leading-[1.45] text-muted-foreground hover:border-primary/50 hover:text-primary", className)}
          />
        }
      >
        {id}
      </HoverCardTrigger>
      <HoverCardContent align="start" className="w-72">
        <SessionPreview id={id} />
      </HoverCardContent>
    </HoverCard>
  );
}

/** The hover card's body: the session, asked for once shown. */
export function SessionPreview({ id }: { id: string }) {
  const { s, session, pending } = useSession(id);
  if (pending || !s) return <p className="font-mono text-[11.5px] text-faint">{id}…</p>;
  if (!session) return <p className="text-[12px] text-muted-foreground">No session is named {id}.</p>;
  const now = Number(s.now);
  return (
    <div className="space-y-2 text-[12px]">
      <div className="flex items-baseline justify-between gap-2">
        <Link to="/sessions/$id" params={{ id }} className="font-mono text-[13px] font-medium text-foreground hover:text-primary">
          {id}
        </Link>
        <SessionStateLabel s={s} session={session} />
      </div>
      <p className="text-muted-foreground">
        {session.schedule ? `a run of the scheduled command ${session.schedule}` : session.agent}
        {session.model && (
          <>
            {" · "}
            <span className="font-mono text-foreground/85">{session.model}</span>
          </>
        )}
      </p>
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-[11px] text-faint">
        {session.project && <ProjectTag name={session.project} />}
        <span>started {ago(session.startedAt, now)}</span>
        <span>{session.endedAt !== undefined ? `ended ${ago(session.endedAt, now)}` : `active ${ago(session.lastActiveAt, now)}`}</span>
      </div>
      <T3ThreadButton session={session} />
    </div>
  );
}

/** Opens the session's T3 Code thread; names it unlinked when T3 Code gave no link. */
function T3ThreadButton({ session }: { session: Session }) {
  if (!session.t3codeThreadId) return null;
  const body = (
    <>
      <T3Mark className="text-[15px]" />
      <span className="min-w-0 flex-1 truncate">{session.t3codeThreadUrl ? "Open in T3 Code" : `T3 Code thread ${session.t3codeThreadId.slice(-8)}`}</span>
      {session.t3codeThreadUrl && <span aria-hidden="true">↗</span>}
    </>
  );
  const box = "flex items-center gap-2 rounded-md border border-border px-2 py-1.5 text-[12px]";
  if (!session.t3codeThreadUrl) {
    return (
      <div className={`${box} text-muted-foreground`} title={`T3 Code thread ${session.t3codeThreadId}`}>
        {body}
      </div>
    );
  }
  return (
    <a
      href={session.t3codeThreadUrl}
      target="_blank"
      rel="noreferrer"
      title={`Open T3 Code thread ${session.t3codeThreadId}`}
      className={`${box} text-foreground hover:border-primary/50 hover:text-primary`}
    >
      {body}
    </a>
  );
}

const STATE_LABEL: Record<SessionState, { text: string; className: string }> = {
  working: { text: "● working", className: "text-primary" },
  idle: { text: "○ idle", className: "text-sprout" },
  ended: { text: "ended", className: "text-faint" },
};

/** Whether a session is working, idle or over. */
export function SessionStateLabel({ s, session }: { s: Snapshot; session: Session }) {
  const st = STATE_LABEL[sessionState(s, session)];
  return <span className={`font-mono text-[11px] whitespace-nowrap ${st.className}`}>{st.text}</span>;
}
