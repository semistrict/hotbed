import { createRootRoute, createRoute, createRouter, redirect } from "@tanstack/react-router";
import type { CommandStateSlug } from "./api";
import { Shell } from "./components/Shell";
import { CommandPage } from "./pages/Command";
import { CommandsPage } from "./pages/Commands";
import { DaemonLogsPage, type DaemonLogsSearch } from "./pages/DaemonLogs";
import { NodePage } from "./pages/Node";
import { NodesPage } from "./pages/Nodes";
import { NotFound } from "./pages/NotFound";
import { OverviewPage } from "./pages/Overview";
import { ProjectsPage } from "./pages/Projects";
import { MemoriesPage, MemoryDetailPage, type MemoriesSearch } from "./pages/Memories";
import { QuestionsPage, type QuestionsSearch } from "./pages/Questions";
import { ReviewPage } from "./pages/Review";
import { ReviewsPage } from "./pages/Reviews";
import { SessionPage } from "./pages/Session";
import { SchedulesPage } from "./pages/Schedules";
import { SessionsPage } from "./pages/Sessions";
import { WorkspacePage } from "./pages/Workspace";

const root = createRootRoute({ component: Shell, notFoundComponent: NotFound });

// Projects are the cockpit's home: what sessions work on, what questions are
// about, and the repos and workspaces under them. The project chosen scopes
// every page but the machines' own.
const projects = createRoute({ getParentRoute: () => root, path: "/", component: ProjectsPage });
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
const MEMORY_TABS = ["proposals", "approved", "history", "all"];
const MEMORY_SCOPES = ["all", "global", "project"];
const MEMORY_MODES = ["all", "always", "on-demand"];
const MEMORY_SORTS = ["rank", "newest", "oldest", "updated", "slug"];
const memories = createRoute({
  getParentRoute: () => root,
  path: "/memories",
  component: MemoriesPage,
  validateSearch: (s: Record<string, unknown>): MemoriesSearch => ({
    tab: typeof s.tab === "string" && MEMORY_TABS.includes(s.tab) ? (s.tab as MemoriesSearch["tab"]) : undefined,
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
const overview = createRoute({ getParentRoute: () => root, path: "/cluster", component: OverviewPage });
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
const command = createRoute({
  getParentRoute: () => root,
  path: "/commands/$id",
  component: CommandPage,
  validateSearch: (s: Record<string, unknown>): { sample?: boolean; pid?: number } => ({
    sample: s.sample === true || s.sample === "1" || s.sample === "true" ? true : undefined,
    pid: typeof s.pid === "number" && s.pid > 0 ? s.pid : typeof s.pid === "string" && Number(s.pid) > 0 ? Number(s.pid) : undefined,
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

const routeTree = root.addChildren([projects, sessions, session, questions, memories, memoryDetail, reviews, review, schedules, overview, nodes, node, daemonLogs, workspaces, workspace, commands, command, history]);

export const router = createRouter({ routeTree, defaultPreload: "intent", scrollRestoration: true });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}

export { command, commands, history, node, workspace };
