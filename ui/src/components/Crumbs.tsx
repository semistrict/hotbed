import { Link } from "@tanstack/react-router";
import { Fragment } from "react";
import { Breadcrumb, BreadcrumbItem, BreadcrumbList, BreadcrumbPage, BreadcrumbSeparator } from "@/components/ui/breadcrumb";
import { useSnapshot } from "../lib/data";
import { useProject } from "../lib/project";
import { repoProject } from "../lib/projects";

export type CrumbSection = {
  label: string;
  to: "/" | "/commands" | "/sessions" | "/reviews" | "/schedules" | "/questions" | "/nodes";
};

/** Where a page sits in the cockpit: its section, project, workspace, and repo. */
export interface Trail {
  /** Top-level section crumb; defaults to Projects (/). */
  section?: CrumbSection;
  /** Its project; else the one holding repoId. */
  project?: string | null;
  workspaceId?: string;
  repoId?: string | null;
  /** The page itself, unlinked. */
  here: string;
}

const link = "text-muted-foreground transition-colors hover:text-primary";

/** section › project › workspace › repo › here, navigating cleanly without overwriting project scope. */
export function Crumbs({ section, project: named, workspaceId, repoId, here }: Trail) {
  const { data: s } = useSnapshot();
  const [chosen] = useProject();
  const project = named || (s && repoId ? repoProject(s, repoId) : null);
  const root: CrumbSection = section ?? { label: "projects", to: "/" };
  const steps = [
    <Link key="section" to={root.to} className={link}>
      {root.label}
    </Link>,
    project &&
      (chosen === project ? (
        <Link key="project" to="/" className={link}>
          {project}
        </Link>
      ) : (
        <span key="project" className="text-faint">
          {project}
        </span>
      )),
    workspaceId && (
      <Link key="ws" to="/workspaces/$id" params={{ id: workspaceId }} className={link}>
        {workspaceId}
      </Link>
    ),
    repoId && (
      <Link key="repo" to="/commands" search={{ repo: repoId }} className={link}>
        {repoId}
      </Link>
    ),
  ].filter(Boolean);
  return (
    <Breadcrumb className="rise min-w-0">
      <BreadcrumbList className="flex-wrap gap-1.5 font-mono text-[11px] text-faint">
        {steps.map((step, i) => (
          <Fragment key={i}>
            <BreadcrumbItem className="min-w-0">{step}</BreadcrumbItem>
            <BreadcrumbSeparator className="text-faint" />
          </Fragment>
        ))}
        <BreadcrumbItem className="min-w-0">
          <BreadcrumbPage className="break-all font-mono text-[11px] text-foreground">{here}</BreadcrumbPage>
        </BreadcrumbItem>
      </BreadcrumbList>
    </Breadcrumb>
  );
}
