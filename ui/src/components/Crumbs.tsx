import { Link } from "@tanstack/react-router";
import { Fragment } from "react";

/** Where a page sits under Workspaces: its repo, then its workspace. */
export interface Trail {
  repoId?: string;
  workspaceId?: string;
  /** The page itself, unlinked. */
  here: string;
}

const link = "text-muted-foreground hover:text-primary";

/** Workspaces › repo › workspace › here, each a way back up. */
export function Crumbs({ repoId, workspaceId, here }: Trail) {
  const steps = [
    <Link key="all" to="/workspaces" className={link}>
      workspaces
    </Link>,
    repoId && (
      <Link key="repo" to="/workspaces" search={{ repo: repoId }} className={link}>
        {repoId}
      </Link>
    ),
    workspaceId && (
      <Link key="ws" to="/workspaces/$id" params={{ id: workspaceId }} className={link}>
        {workspaceId}
      </Link>
    ),
  ].filter(Boolean);
  return (
    <nav aria-label="Breadcrumb" className="rise flex flex-wrap items-center gap-1.5 font-mono text-[11px] text-faint">
      {steps.map((s, i) => (
        <Fragment key={i}>
          {s}
          <span aria-hidden>›</span>
        </Fragment>
      ))}
      <span aria-current="page" className="text-foreground">
        {here}
      </span>
    </nav>
  );
}
