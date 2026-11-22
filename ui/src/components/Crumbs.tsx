import { Link, useNavigate } from "@tanstack/react-router";
import { Fragment } from "react";
import { useSnapshot } from "../lib/data";
import { useProject } from "../lib/project";
import { repoProject } from "../lib/projects";

/** Where a page sits under Projects: its project, repo, and workspace. */
export interface Trail {
  repoId?: string;
  workspaceId?: string;
  /** The page itself, unlinked. */
  here: string;
}

const link = "text-muted-foreground hover:text-primary";

/** Projects › project › repo › workspace › here, each a way back up. */
export function Crumbs({ repoId, workspaceId, here }: Trail) {
  const { data: s } = useSnapshot();
  const [, choose] = useProject();
  const navigate = useNavigate();
  const project = s && repoId ? repoProject(s, repoId) : null;
  const steps = [
    <Link key="all" to="/" onClick={() => choose(null)} className={link}>
      projects
    </Link>,
    project && (
      <button
        key="project"
        type="button"
        onClick={() => {
          choose(project);
          void navigate({ to: "/" });
        }}
        className={link}
      >
        {project}
      </button>
    ),
    repoId && (
      <span key="repo" className="text-muted-foreground">
        {repoId}
      </span>
    ),
    workspaceId && (
      <Link key="ws" to="/workspaces/$id" params={{ id: workspaceId }} className={link}>
        {workspaceId}
      </Link>
    ),
  ].filter(Boolean);
  return (
    <nav aria-label="Breadcrumb" className="rise flex flex-wrap items-center gap-1.5 font-mono text-[11px] text-faint">
      {steps.map((step, i) => (
        <Fragment key={i}>
          {step}
          <span aria-hidden>›</span>
        </Fragment>
      ))}
      <span aria-current="page" className="text-foreground">
        {here}
      </span>
    </nav>
  );
}
