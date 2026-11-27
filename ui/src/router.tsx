import { createRootRoute, createRoute, createRouter, redirect } from "@tanstack/react-router";
import type { CommandStateSlug } from "./api";
import { Shell } from "./components/Shell";
import { keepScrollWithinPage } from "./lib/scroll";
import { AdminAgentPage } from "./pages/AdminAgent";
import { BoardPage, BoardThreadPage, type BoardSearch } from "./pages/Board";
import { CommandPage } from "./pages/Command";
import { CommandsPage } from "./pages/Commands";
import { DaemonLogsPage, type DaemonLogsSearch } from "./pages/DaemonLogs";
import { NodePage } from "./pages/Node";
import { NodesPage } from "./pages/Nodes";
import { NotFound } from "./pages/NotFound";
import { OrganizationPage } from "./pages/Organization";
import { OverviewPage } from "./pages/Overview";
import { ProjectsPage } from "./pages/Projects";
import { MemoriesPage, MemoryDetailPage, type MemoriesSearch } from "./pages/Memories";
import { SkillDetailPage, SkillsPage, type SkillsSearch } from "./pages/Skills";
import { QuestionDetailPage, QuestionsPage, type QuestionsSearch } from "./pages/Questions";
import { IssueDetailPage, IssuesPage, type IssuesSearch, type IssuesTab } from "./pages/Issues";
import { ReviewPage } from "./pages/Review";
import { ReviewsPage } from "./pages/Reviews";
import { SessionPage } from "./pages/Session";
import { SchedulesPage } from "./pages/Schedules";
import { SessionsPage } from "./pages/Sessions";
import { SettingsPage, type SettingsSearch, type SettingsTab } from "./pages/Settings";
import { TasksPage, type TasksSearch, type TasksTab } from "./pages/Tasks";
import { WorkspacePage } from "./pages/Workspace";

const root = createRootRoute({ component: Shell, notFoundComponent: NotFound });

// Projects are the cockpit's home: what sessions work on, what questions are
// about, and the repos and workspaces under them. The project chosen scopes
// every page but the machines' own.
const projects = createRoute({ getParentRoute: () => root, path: "/", component: ProjectsPage });
const board = createRoute({
  getParentRoute: () => root,
  path: "/board",
  component: BoardPage,
  validateSearch: (s: Record<string, unknown>): BoardSearch => ({
    q: typeof s.q === "string" && s.q ? s.q : undefined,
    label: typeof s.label === "string" && s.label ? s.label : undefined,
    by: typeof s.by === "string" && s.by ? s.by : undefined,
  }),
});
const boardThread = createRoute({
  getParentRoute: () => root,
  path: "/board/$id",
  component: BoardThreadPage,
});
const TASK_TABS: readonly TasksTab[] = ["open", "ready", "claimed", "blocked", "waiting", "completed", "failed", "canceled", "all"];
const tasks = createRoute({
  getParentRoute: () => root,
  path: "/tasks",
  component: TasksPage,
  validateSearch: (s: Record<string, unknown>): TasksSearch => ({
    tab: typeof s.tab === "string" && TASK_TABS.includes(s.tab as TasksTab) ? (s.tab as TasksTab) : undefined,
  }),
});
const sessions = createRoute({ getParentRoute: () => root, path: "/sessions", component: SessionsPage });
const session = createRoute({ getParentRoute: () => root, path: "/sessions/$id", component: SessionPage });
const reviews = createRoute({ getParentRoute: () => root, path: "/reviews", component: ReviewsPage });
const review = createRoute({ getParentRoute: () => root, path: "/reviews/$id", component: ReviewPage });
const schedules = createRoute({ getParentRoute: () => root, path: "/schedules", component: SchedulesPage });
const TABS = ["inbox", "outbox", "handled", "deleted", "all"];
const SORTS = ["rank", "newest", "oldest", "updated"];
const questions = createRoute({
  getParentRoute: () => root,
  path: "/questions",
  component: QuestionsPage,
  validateSearch: (s: Record<string, unknown>): QuestionsSearch => ({
    tab: typeof s.tab === "string" && TABS.includes(s.tab) ? (s.tab as QuestionsSearch["tab"]) : undefined,
    sort: typeof s.sort === "string" && SORTS.includes(s.sort) ? (s.sort as QuestionsSearch["sort"]) : undefined,
  }),
});
const ISSUE_TABS: readonly IssuesTab[] = ["open", "archived", "all"];
const issues = createRoute({
  getParentRoute: () => root,
  path: "/issues",
  component: IssuesPage,
  validateSearch: (s: Record<string, unknown>): IssuesSearch => ({
    tab: typeof s.tab === "string" && ISSUE_TABS.includes(s.tab as IssuesTab) ? (s.tab as IssuesTab) : undefined,
  }),
});
const issueDetail = createRoute({ getParentRoute: () => root, path: "/issues/$id", component: IssueDetailPage });
const questionDetail = createRoute({ getParentRoute: () => root, path: "/questions/$id", component: QuestionDetailPage });
const MEMORY_TABS = ["active", "history", "all"];
const MEMORY_SCOPES = ["all", "global", "project"];
const MEMORY_MODES = ["all", "always", "on-demand"];
const MEMORY_SORTS = ["rank", "newest", "oldest", "updated", "slug"];
const memories = createRoute({
  getParentRoute: () => root,
  path: "/memories",
  component: MemoriesPage,
  validateSearch: (s: Record<string, unknown>): MemoriesSearch => ({
    tab:
      s.tab === "approved"
        ? "active"
        : typeof s.tab === "string" && MEMORY_TABS.includes(s.tab)
          ? (s.tab as MemoriesSearch["tab"])
          : undefined,
    scope: typeof s.scope === "string" && MEMORY_SCOPES.includes(s.scope) ? (s.scope as MemoriesSearch["scope"]) : undefined,
    mode: typeof s.mode === "string" && MEMORY_MODES.includes(s.mode) ? (s.mode as MemoriesSearch["mode"]) : undefined,
    sort: typeof s.sort === "string" && MEMORY_SORTS.includes(s.sort) ? (s.sort as MemoriesSearch["sort"]) : undefined,
    q: typeof s.q === "string" && s.q ? s.q : undefined,
  }),
});
const memoryDetail = createRoute({
  getParentRoute: () => root,
  path: "/memories/$id",
  component: MemoryDetailPage,
});
const SKILL_TABS = ["active", "archived", "all"];
const SKILL_SCOPES = ["all", "global", "project"];
const SKILL_SORTS = ["rank", "newest", "oldest", "updated", "name"];
const skills = createRoute({
  getParentRoute: () => root,
  path: "/skills",
  component: SkillsPage,
  validateSearch: (s: Record<string, unknown>): SkillsSearch => ({
    tab: typeof s.tab === "string" && SKILL_TABS.includes(s.tab) ? (s.tab as SkillsSearch["tab"]) : undefined,
    scope: typeof s.scope === "string" && SKILL_SCOPES.includes(s.scope) ? (s.scope as SkillsSearch["scope"]) : undefined,
    sort: typeof s.sort === "string" && SKILL_SORTS.includes(s.sort) ? (s.sort as SkillsSearch["sort"]) : undefined,
    q: typeof s.q === "string" && s.q ? s.q : undefined,
  }),
});
const skillDetail = createRoute({
  getParentRoute: () => root,
  path: "/skills/$id",
  component: SkillDetailPage,
});
const overview = createRoute({ getParentRoute: () => root, path: "/cluster", component: OverviewPage });
const adminExplorer = createRoute({ getParentRoute: () => root, path: "/admin-explorer", component: AdminAgentPage });
const adminAgent = createRoute({
  getParentRoute: () => root,
  path: "/admin-agent",
  beforeLoad: () => {
    throw redirect({ to: "/admin-explorer" });
  },
});
const organization = createRoute({ getParentRoute: () => root, path: "/org", component: OrganizationPage });
const SETTINGS_TABS: readonly SettingsTab[] = ["freshen", "runtime", "vm", "fuse", "system", "git", "json"];
const settings = createRoute({
  getParentRoute: () => root,
  path: "/settings",
  component: SettingsPage,
  validateSearch: (s: Record<string, unknown>): SettingsSearch => ({
    scope: typeof s.scope === "string" && s.scope.trim() ? s.scope.trim() : undefined,
    tab: typeof s.tab === "string" && SETTINGS_TABS.includes(s.tab as SettingsTab) ? (s.tab as SettingsTab) : undefined,
  }),
});
const nodes = createRoute({ getParentRoute: () => root, path: "/nodes", component: NodesPage });
const node = createRoute({ getParentRoute: () => root, path: "/nodes/$hostname", component: NodePage });
const LOG_SINCELIST = ["15m", "1h", "24h", "7d", "all"];
const daemonLogs = createRoute({
  getParentRoute: () => root,
  path: "/logs",
  component: DaemonLogsPage,
  validateSearch: (s: Record<string, unknown>): DaemonLogsSearch => ({
    source: typeof s.source === "string" && s.source ? s.source : undefined,
    level: s.level === "warn" || s.level === "error" ? s.level : undefined,
    since: typeof s.since === "string" && LOG_SINCELIST.includes(s.since) ? (s.since as DaemonLogsSearch["since"]) : undefined,
  }),
});

// Workspaces live under Projects -> Repos; keep /workspaces redirecting cleanly
// and /workspaces/$id as the detail view.
const workspaces = createRoute({
  getParentRoute: () => root,
  path: "/workspaces",
  beforeLoad: () => {
    throw redirect({ to: "/" });
  },
});
const workspace = createRoute({
  getParentRoute: () => root,
  path: "/workspaces/$id",
  component: WorkspacePage,
  validateSearch: (s: Record<string, unknown>): { file?: string } => (typeof s.file === "string" ? { file: s.file } : {}),
});

export interface CommandsSearch {
  state?: CommandStateSlug | "live";
  node?: string;
  repo?: string;
  outcome?: "failed" | "succeeded";
}

const commands = createRoute({
  getParentRoute: () => root,
  path: "/commands",
  component: CommandsPage,
  validateSearch: (s: Record<string, unknown>): CommandsSearch => ({
    state: typeof s.state === "string" ? (s.state as CommandsSearch["state"]) : undefined,
    node: typeof s.node === "string" ? s.node : undefined,
    repo: typeof s.repo === "string" ? s.repo : undefined,
    outcome: s.outcome === "failed" || s.outcome === "succeeded" ? s.outcome : undefined,
  }),
});
/**
 * A command page's place: sample, the kept sample it shows (else its
 * overview); take, to take a sample as it opens, of pid if given.
 */
export interface CommandSearch {
  sample?: number;
  take?: true;
  pid?: number;
}

function positive(v: unknown): number | undefined {
  const n = typeof v === "number" ? v : typeof v === "string" ? Number(v) : NaN;
  return Number.isInteger(n) && n > 0 ? n : undefined;
}

const command = createRoute({
  getParentRoute: () => root,
  path: "/commands/$id",
  component: CommandPage,
  validateSearch: (s: Record<string, unknown>): CommandSearch => ({
    sample: positive(s.sample),
    take: s.take === true || s.take === "1" || s.take === "true" ? true : undefined,
    pid: positive(s.pid),
  }),
});

// History merged into Commands; redirect old links preserving repo/outcome filters.
const history = createRoute({
  getParentRoute: () => root,
  path: "/history",
  validateSearch: (s: Record<string, unknown>): Pick<CommandsSearch, "repo" | "outcome"> => ({
    repo: typeof s.repo === "string" ? s.repo : undefined,
    outcome: s.outcome === "failed" || s.outcome === "succeeded" ? s.outcome : undefined,
  }),
  beforeLoad: ({ search }) => {
    throw redirect({ to: "/commands", search });
  },
});

const routeTree = root.addChildren([projects, board, boardThread, tasks, sessions, session, questions, questionDetail, issues, issueDetail, memories, memoryDetail, skills, skillDetail, reviews, review, schedules, overview, adminExplorer, adminAgent, organization, settings, nodes, node, daemonLogs, workspaces, workspace, commands, command, history]);

export const router = createRouter({ routeTree, defaultPreload: "intent", scrollRestoration: true });
keepScrollWithinPage(router);

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}

export { command, commands, history, node, workspace };
