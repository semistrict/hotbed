import { createRootRoute, createRoute, createRouter } from "@tanstack/react-router";
import type { CommandState } from "./api";
import { Shell } from "./components/Shell";
import { CommandPage } from "./pages/Command";
import { CommandsPage } from "./pages/Commands";
import { HistoryPage } from "./pages/History";
import { NodePage } from "./pages/Node";
import { NodesPage } from "./pages/Nodes";
import { NotFound } from "./pages/NotFound";
import { OverviewPage } from "./pages/Overview";
import { SessionPage } from "./pages/Session";
import { SchedulesPage } from "./pages/Schedules";
import { SessionsPage } from "./pages/Sessions";
import { WorkspacePage } from "./pages/Workspace";
import { WorkspacesPage } from "./pages/Workspaces";

const root = createRootRoute({ component: Shell, notFoundComponent: NotFound });

// Sessions are what an agent works in, so they are the cockpit's home; the
// cluster's own view is a page of its own.
const sessions = createRoute({ getParentRoute: () => root, path: "/", component: SessionsPage });
const session = createRoute({ getParentRoute: () => root, path: "/sessions/$id", component: SessionPage });
const schedules = createRoute({ getParentRoute: () => root, path: "/schedules", component: SchedulesPage });
const overview = createRoute({ getParentRoute: () => root, path: "/cluster", component: OverviewPage });
const nodes = createRoute({ getParentRoute: () => root, path: "/nodes", component: NodesPage });
const node = createRoute({ getParentRoute: () => root, path: "/nodes/$hostname", component: NodePage });
export interface WorkspacesSearch {
  repo?: string;
  q?: string;
}

const workspaces = createRoute({
  getParentRoute: () => root,
  path: "/workspaces",
  component: WorkspacesPage,
  validateSearch: (s: Record<string, unknown>): WorkspacesSearch => ({
    repo: typeof s.repo === "string" ? s.repo : undefined,
    q: typeof s.q === "string" && s.q !== "" ? s.q : undefined,
  }),
});
const workspace = createRoute({
  getParentRoute: () => root,
  path: "/workspaces/$id",
  component: WorkspacePage,
  validateSearch: (s: Record<string, unknown>): { file?: string } => (typeof s.file === "string" ? { file: s.file } : {}),
});

export interface CommandsSearch {
  state?: CommandState | "live";
  node?: string;
}

const commands = createRoute({
  getParentRoute: () => root,
  path: "/commands",
  component: CommandsPage,
  validateSearch: (s: Record<string, unknown>): CommandsSearch => ({
    state: typeof s.state === "string" ? (s.state as CommandsSearch["state"]) : undefined,
    node: typeof s.node === "string" ? s.node : undefined,
  }),
});
const command = createRoute({ getParentRoute: () => root, path: "/commands/$id", component: CommandPage });

export interface HistorySearch {
  repo?: string;
  outcome?: "failed" | "succeeded";
}

const history = createRoute({
  getParentRoute: () => root,
  path: "/history",
  component: HistoryPage,
  validateSearch: (s: Record<string, unknown>): HistorySearch => ({
    repo: typeof s.repo === "string" ? s.repo : undefined,
    outcome: s.outcome === "failed" || s.outcome === "succeeded" ? s.outcome : undefined,
  }),
});

const routeTree = root.addChildren([sessions, session, schedules, overview, nodes, node, workspaces, workspace, commands, command, history]);

export const router = createRouter({ routeTree, defaultPreload: "intent", scrollRestoration: true });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}

export { command, commands, history, node, workspace };
