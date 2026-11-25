import { Link, Outlet, useNavigate, useRouterState } from "@tanstack/react-router";
import {
  Activity,
  Brain,
  Building2,
  CalendarClock,
  ChevronsUpDown,
  Cpu,
  FolderKanban,
  GitPullRequestDraft,
  Layers,
  LogOut,
  MessageCircleQuestion,
  Monitor,
  Moon,
  ScrollText,
  Server,
  Sun,
  Terminal,
  X,
  type LucideIcon,
} from "lucide-react";
import { useEffect, useState, type ReactNode } from "react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Select, SelectContent, SelectItem, SelectSeparator, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Separator } from "@/components/ui/separator";
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupContent,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarInset,
  SidebarMenu,
  SidebarMenuBadge,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarProvider,
  SidebarRail,
  SidebarSeparator,
  SidebarTrigger,
  useSidebar,
} from "@/components/ui/sidebar";
import type { Snapshot } from "../api";
import { signOut, useAuthMe, useSwitchOrganization } from "../lib/auth";
import { useSnapshot } from "../lib/data";
import { isLive } from "../lib/live";
import { useProject, useScopedSnapshot } from "../lib/project";

interface NavItem {
  readonly to:
    | "/"
    | "/questions"
    | "/memories"
    | "/reviews"
    | "/sessions"
    | "/commands"
    | "/schedules"
    | "/cluster"
    | "/nodes"
    | "/logs"
    | "/org";
  readonly label: string;
  readonly icon: LucideIcon;
  readonly shared?: boolean;
}

// A project's work first, as the chosen project sees it; then the machines,
// which every project shares.
const WORKSPACE_NAV: readonly NavItem[] = [
  { to: "/", label: "Projects", icon: FolderKanban },
  { to: "/questions", label: "Questions", icon: MessageCircleQuestion },
  { to: "/memories", label: "Memories", icon: Brain },
  { to: "/reviews", label: "Reviews", icon: GitPullRequestDraft },
  { to: "/sessions", label: "Sessions", icon: Terminal },
  { to: "/commands", label: "Commands", icon: Cpu },
  { to: "/schedules", label: "Schedules", icon: CalendarClock },
];

const INFRA_NAV: readonly NavItem[] = [
  { to: "/cluster", label: "Cluster", icon: Activity, shared: true },
  { to: "/nodes", label: "Nodes", icon: Server, shared: true },
  { to: "/logs", label: "Logs", icon: ScrollText, shared: true },
  { to: "/org", label: "Organization", icon: Building2, shared: true },
];

const ALL_NAV: readonly NavItem[] = [...WORKSPACE_NAV, ...INFRA_NAV];

export function isNavActive(pathname: string, to: NavItem["to"]): boolean {
  if (to === "/") {
    return pathname === "/" || pathname.startsWith("/workspaces");
  }
  return pathname === to || pathname.startsWith(`${to}/`);
}

export function routeTitle(pathname: string): string {
  if (pathname.startsWith("/workspaces")) return "Workspace";
  const found = ALL_NAV.find((n) => isNavActive(pathname, n.to));
  return found?.label ?? "hotbed";
}

type Theme = "system" | "light" | "dark";

function useTheme(): [Theme, (next: Theme) => void, () => void] {
  const [theme, setTheme] = useState<Theme>(() => {
    try {
      const saved = localStorage.getItem("hotbed-theme");
      return saved === "light" || saved === "dark" || saved === "system" ? saved : "system";
    } catch {
      return "system";
    }
  });
  useEffect(() => {
    const root = document.documentElement;
    if (theme === "system") root.removeAttribute("data-theme");
    else root.setAttribute("data-theme", theme);
    try {
      localStorage.setItem("hotbed-theme", theme);
    } catch {
      // Private windows refuse storage; the theme still applies for this visit.
    }
  }, [theme]);
  const cycle = () => setTheme((t) => (t === "system" ? "dark" : t === "dark" ? "light" : "system"));
  return [theme, setTheme, cycle];
}

function readInitialSidebarOpen(): boolean {
  if (typeof document === "undefined") return true;
  const match = document.cookie.match(/(?:^|;\s*)sidebar_state=(true|false)/);
  return match ? match[1] === "true" : true;
}

export function Shell() {
  const [defaultOpen] = useState(readInitialSidebarOpen);
  return (
    <SidebarProvider defaultOpen={defaultOpen}>
      <ShellFrame />
    </SidebarProvider>
  );
}

function ShellFrame() {
  const { data: whole, isError, error } = useSnapshot();
  const { data } = useScopedSnapshot();
  const [project, chooseProject] = useProject();
  const [theme, setTheme, cycleTheme] = useTheme();
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const { setOpenMobile } = useSidebar();
  const online = whole?.nodes.filter((n) => n.online).length ?? 0;

  // Close the mobile navigation drawer whenever the route changes.
  useEffect(() => {
    setOpenMobile(false);
  }, [pathname, setOpenMobile]);

  const currentLabel = routeTitle(pathname);
  const openQuestions = data?.openQuestions ?? 0;

  return (
    <>
      <AppSidebar
        whole={whole}
        scoped={data}
        pathname={pathname}
        theme={theme}
        setTheme={setTheme}
        cycleTheme={cycleTheme}
        online={online}
      />

      <SidebarInset className="min-w-0 bg-transparent">
        <header className="sticky top-0 z-30 flex h-13 shrink-0 items-center gap-2 border-b border-border bg-background/80 px-3 backdrop-blur-md sm:h-12 sm:px-5 lg:px-7">
          <SidebarTrigger className="size-9 shrink-0 text-muted-foreground hover:text-foreground md:size-7" />
          <Separator orientation="vertical" className="mr-0.5 h-4 bg-border" />

          <div className="flex min-w-0 items-center gap-2">
            <Link to="/" className="flex shrink-0 items-center gap-1.5 md:hidden" aria-label="hotbed home">
              <Logo size={20} />
            </Link>
            <span className="truncate text-[13px] font-medium text-foreground">{currentLabel}</span>
            {project && (
              <span className="hidden items-center gap-1 rounded-full border border-primary/35 bg-ember-wash px-2 py-0.5 font-mono text-[10.5px] text-primary sm:inline-flex">
                <span aria-hidden>◆</span>
                <span className="max-w-32 truncate">{project}</span>
                <button
                  type="button"
                  onClick={() => chooseProject(null)}
                  className="ml-0.5 rounded-full p-0.5 text-primary/80 transition-colors hover:bg-primary/15 hover:text-primary"
                  aria-label="Clear project scope"
                  title="Show every project"
                >
                  <X className="size-2.5" />
                </button>
              </span>
            )}
          </div>

          <div className="ml-auto flex shrink-0 items-center gap-2">
            {/* On mobile, expose a quick compact project switcher right in the header */}
            <div className="md:hidden">
              <ProjectPicker compact />
            </div>

            {openQuestions > 0 && pathname !== "/questions" && (
              <Link
                to="/questions"
                className="inline-flex h-7 items-center gap-1 rounded-full border border-primary/40 bg-ember-wash px-2 font-mono text-[10.5px] text-primary transition-colors hover:bg-primary/20 md:hidden"
                aria-label={`${openQuestions} questions waiting for you`}
              >
                <MessageCircleQuestion className="size-3.5" />
                <span>{openQuestions}</span>
              </Link>
            )}

            {whole && (
              <Link
                to="/nodes"
                className="hidden items-center gap-2 rounded-full border border-border bg-card/60 px-2.5 py-1 font-mono text-[10.5px] text-muted-foreground transition-colors hover:border-border-strong hover:text-foreground sm:inline-flex"
                title={`${whole.controller} · ${online} of ${whole.nodes.length} nodes online`}
              >
                <span className="live-dot inline-block size-1.5 rounded-full bg-primary text-primary" />
                <span className="max-w-36 truncate">{whole.controller}</span>
                <span className="text-faint">·</span>
                <span className="num text-foreground">
                  {online}/{whole.nodes.length}
                </span>
              </Link>
            )}
          </div>
        </header>

        <div className="mx-auto w-full max-w-[1440px] flex-1 px-3.5 pt-4 pb-16 sm:px-6 sm:pt-6 lg:px-8">
          {whole ? (
            <>
              {isError && (
                <Alert variant="destructive" className="mb-4 rounded-xl border-critical/40 bg-card px-4 py-2.5 font-mono text-[11.5px] text-critical">
                  <AlertDescription className="text-[11.5px] text-critical">
                    Lost connection to the controller — showing last snapshot ({String(error)})
                  </AlertDescription>
                </Alert>
              )}
              <Outlet />
            </>
          ) : isError ? (
            <Alert variant="destructive" className="rounded-xl border-critical/40 bg-card p-6 font-mono text-[12px] text-critical">
              <AlertDescription className="text-[12px] text-critical">Cannot reach the controller: {String(error)}</AlertDescription>
            </Alert>
          ) : (
            <div className="py-24 text-center font-mono text-[12px] text-muted-foreground">warming up…</div>
          )}
        </div>
      </SidebarInset>
    </>
  );
}

function AppSidebar({
  whole,
  scoped,
  pathname,
  theme,
  setTheme,
  cycleTheme,
  online,
}: {
  whole: Snapshot | undefined;
  scoped: Snapshot | undefined;
  pathname: string;
  theme: Theme;
  setTheme: (next: Theme) => void;
  cycleTheme: () => void;
  online: number;
}) {
  const [project] = useProject();
  const { setOpenMobile } = useSidebar();

  const reviewsToDecide = scoped
    ? scoped.projects.filter((p) => project === null || p.name === project).reduce((sum, p) => sum + p.reviewsToDecide, 0)
    : 0;
  const liveSessions = scoped ? scoped.sessions.filter((x) => x.endedAt === undefined).length : 0;
  const liveCommands = scoped ? scoped.commands.filter(isLive).length : 0;
  const logErrors = whole ? (whole.controllerLogs?.errors24h ?? 0) + whole.nodes.reduce((sum, n) => sum + (n.daemonLogs?.errors24h ?? 0), 0) : 0;
  const logWarns = whole ? (whole.controllerLogs?.warns24h ?? 0) + whole.nodes.reduce((sum, n) => sum + (n.daemonLogs?.warns24h ?? 0), 0) : 0;

  const badgeFor = (to: NavItem["to"]): { node: ReactNode; urgent?: boolean; tooltipSuffix?: string } => {
    if (to === "/questions" && scoped && scoped.openQuestions > 0) {
      return {
        node: (
          <Badge className="h-4 min-w-4 px-1.5 font-mono text-[10px] leading-4" aria-label={`${scoped.openQuestions} waiting for you`}>
            {scoped.openQuestions}
          </Badge>
        ),
        urgent: true,
        tooltipSuffix: ` (${scoped.openQuestions} waiting)`,
      };
    }
    if (to === "/memories" && scoped && scoped.pendingMemories > 0) {
      return {
        node: (
          <Badge className="h-4 min-w-4 px-1.5 font-mono text-[10px] leading-4" aria-label={`${scoped.pendingMemories} pending proposals`}>
            {scoped.pendingMemories}
          </Badge>
        ),
        urgent: true,
        tooltipSuffix: ` (${scoped.pendingMemories} pending)`,
      };
    }
    if (to === "/reviews" && reviewsToDecide > 0) {
      return {
        node: (
          <Badge variant="outline" className="h-4 min-w-4 border-primary/50 px-1.5 font-mono text-[10px] leading-4 text-primary" aria-label={`${reviewsToDecide} with findings to decide`}>
            {reviewsToDecide}
          </Badge>
        ),
        urgent: true,
        tooltipSuffix: ` (${reviewsToDecide} to decide)`,
      };
    }
    if (to === "/sessions" && liveSessions > 0) {
      return {
        node: <span className="num font-mono text-[10.5px] text-muted-foreground">{liveSessions}</span>,
        tooltipSuffix: ` (${liveSessions} live)`,
      };
    }
    if (to === "/commands" && liveCommands > 0) {
      return {
        node: <span className="num font-mono text-[10.5px] text-primary">{liveCommands}</span>,
        urgent: true,
        tooltipSuffix: ` (${liveCommands} live)`,
      };
    }
    if (to === "/nodes" && whole) {
      return {
        node: (
          <span className="num font-mono text-[10.5px] text-muted-foreground">
            {online}/{whole.nodes.length}
          </span>
        ),
        tooltipSuffix: ` (${online}/${whole.nodes.length} online)`,
      };
    }
    if (to === "/logs" && logErrors > 0) {
      return {
        node: (
          <Badge
            variant="destructive"
            className="h-4 min-w-4 bg-[color-mix(in_oklab,var(--critical)_18%,transparent)] px-1.5 font-mono text-[10px] leading-4 text-critical"
            title={`${logErrors} errors, ${logWarns} warnings in the last 24h`}
          >
            {logErrors}
          </Badge>
        ),
        urgent: true,
        tooltipSuffix: ` (${logErrors} errors)`,
      };
    }
    if (to === "/logs" && logWarns > 0) {
      return {
        node: (
          <Badge
            variant="secondary"
            className="h-4 min-w-4 bg-[color-mix(in_oklab,var(--warning)_22%,transparent)] px-1.5 font-mono text-[10px] leading-4 text-warning"
            title={`${logWarns} warnings in the last 24h`}
          >
            {logWarns}
          </Badge>
        ),
        tooltipSuffix: ` (${logWarns} warnings)`,
      };
    }
    return { node: null };
  };

  const renderNavItems = (items: readonly NavItem[]) =>
    items.map((item) => {
      const active = isNavActive(pathname, item.to);
      const badge = badgeFor(item.to);
      const Icon = item.icon;
      return (
        <SidebarMenuItem key={item.to}>
          <SidebarMenuButton
            isActive={active}
            tooltip={`${item.label}${badge.tooltipSuffix ?? ""}`}
            className="h-9 gap-2.5 rounded-lg px-2.5 text-[13px] text-muted-foreground transition-colors hover:bg-sidebar-accent/80 hover:text-foreground data-active:bg-ember-wash data-active:font-medium data-active:text-foreground data-active:[&_svg]:text-primary md:h-8 md:text-[12.5px]"
            render={
              <Link
                to={item.to}
                title={item.shared ? "Shared across every project" : undefined}
                onClick={() => setOpenMobile(false)}
              />
            }
          >
            <Icon className="size-4 shrink-0" />
            <span>{item.label}</span>
          </SidebarMenuButton>
          {badge.node && <SidebarMenuBadge className="right-2 peer-data-[size=default]/menu-button:top-2 md:peer-data-[size=default]/menu-button:top-1.5">{badge.node}</SidebarMenuBadge>}
          {badge.urgent && (
            <span
              aria-hidden
              className="pointer-events-none absolute top-1.5 right-1.5 hidden size-1.5 rounded-full bg-primary group-data-[collapsible=icon]:block"
            />
          )}
        </SidebarMenuItem>
      );
    });

  return (
    <Sidebar collapsible="icon" className="border-r border-sidebar-border bg-sidebar/95 backdrop-blur-md">
      <SidebarHeader className="gap-2.5 border-b border-sidebar-border/80 px-3 py-3 group-data-[collapsible=icon]:items-center group-data-[collapsible=icon]:px-2">
        <Link
          to="/"
          onClick={() => setOpenMobile(false)}
          className="flex items-center gap-2.5 px-0.5 py-0.5 group-data-[collapsible=icon]:justify-center group-data-[collapsible=icon]:px-0"
        >
          <Logo size={25} />
          <span className="display text-[22px] leading-none font-semibold italic group-data-[collapsible=icon]:hidden">hotbed</span>
        </Link>

        <div className="space-y-1.5 group-data-[collapsible=icon]:hidden">
          <OrgPicker />
          <ProjectPicker />
        </div>
      </SidebarHeader>

      <SidebarContent className="px-1 py-1.5">
        <SidebarGroup className="py-1.5">
          <SidebarGroupLabel className="eyebrow h-7 px-2 text-[10px]">
            {project ? `Project · ${project}` : "Workspace"}
          </SidebarGroupLabel>
          <SidebarGroupContent>
            <SidebarMenu className="gap-0.5">{renderNavItems(WORKSPACE_NAV)}</SidebarMenu>
          </SidebarGroupContent>
        </SidebarGroup>

        <SidebarSeparator className="my-1" />

        <SidebarGroup className="py-1.5">
          <SidebarGroupLabel className="eyebrow h-7 px-2 text-[10px]">Infrastructure</SidebarGroupLabel>
          <SidebarGroupContent>
            <SidebarMenu className="gap-0.5">{renderNavItems(INFRA_NAV)}</SidebarMenu>
          </SidebarGroupContent>
        </SidebarGroup>
      </SidebarContent>

      <SidebarFooter className="gap-2 border-t border-sidebar-border/80 p-2.5 group-data-[collapsible=icon]:items-center group-data-[collapsible=icon]:px-2">
        {whole && (
          <div
            className="flex items-center gap-2 rounded-lg bg-sidebar-accent/55 px-2.5 py-1.5 font-mono text-[10.5px] text-muted-foreground group-data-[collapsible=icon]:justify-center group-data-[collapsible=icon]:bg-transparent group-data-[collapsible=icon]:p-1.5"
            title={`${whole.controller} · ${online}/${whole.nodes.length} nodes online`}
          >
            <span className="live-dot inline-block size-1.5 shrink-0 rounded-full bg-primary text-primary" />
            <span className="min-w-0 flex-1 truncate group-data-[collapsible=icon]:hidden">{whole.controller}</span>
            <span className="num shrink-0 text-foreground group-data-[collapsible=icon]:hidden">
              {online}/{whole.nodes.length}
            </span>
          </div>
        )}

        <ThemeControl theme={theme} setTheme={setTheme} cycleTheme={cycleTheme} />
        <AccountMenu />
      </SidebarFooter>

      <SidebarRail />
    </Sidebar>
  );
}

function Logo({ size = 26 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 32 32" aria-hidden className="shrink-0">
      <path d="M16 25c0-6 0-9 4.5-12.5" stroke="var(--sprout)" strokeWidth="2.4" strokeLinecap="round" fill="none" />
      <path d="M16 25c0-4-1.5-6.5-5.5-8.5" stroke="var(--sprout)" strokeWidth="2.4" strokeLinecap="round" fill="none" />
      <path d="M20.5 12.5c1.8-1.6 4.2-2 5.5-1.6-.3 1.6-1.8 3.4-4.4 3.9" fill="var(--sprout)" />
      <path d="M5 26h22" stroke="var(--primary)" strokeWidth="2.6" strokeLinecap="round" />
      <path d="M8 29.5h16" stroke="var(--primary)" strokeOpacity=".45" strokeWidth="1.6" strokeLinecap="round" />
    </svg>
  );
}

function ThemeControl({
  theme,
  setTheme,
  cycleTheme,
}: {
  theme: Theme;
  setTheme: (next: Theme) => void;
  cycleTheme: () => void;
}) {
  const options: readonly { value: Theme; label: string; icon: LucideIcon }[] = [
    { value: "system", label: "Auto", icon: Monitor },
    { value: "dark", label: "Dark", icon: Moon },
    { value: "light", label: "Light", icon: Sun },
  ];
  const ActiveIcon = options.find((o) => o.value === theme)?.icon ?? Monitor;

  return (
    <>
      <div
        role="group"
        aria-label="Theme"
        className="grid grid-cols-3 gap-0.5 rounded-lg border border-sidebar-border bg-background/50 p-0.5 group-data-[collapsible=icon]:hidden"
      >
        {options.map(({ value, label, icon: Icon }) => {
          const active = theme === value;
          return (
            <button
              key={value}
              type="button"
              onClick={() => setTheme(value)}
              aria-pressed={active}
              className={`flex h-7 items-center justify-center gap-1 rounded-md font-mono text-[10.5px] transition-colors ${
                active ? "bg-sidebar-accent font-medium text-foreground shadow-xs" : "text-muted-foreground hover:text-foreground"
              }`}
            >
              <Icon className="size-3" />
              <span>{label}</span>
            </button>
          );
        })}
      </div>

      <Button
        type="button"
        variant="ghost"
        size="icon-sm"
        onClick={cycleTheme}
        className="hidden size-8 text-muted-foreground hover:text-foreground group-data-[collapsible=icon]:inline-flex"
        title={`Theme: ${theme}`}
        aria-label={`Switch theme (currently ${theme})`}
      >
        <ActiveIcon className="size-4" />
      </Button>
    </>
  );
}

/** Which WorkOS organization the cockpit targets; switching moves to that org's ControllerDO. */
function OrgPicker() {
  const { data: auth } = useAuthMe();
  const switchOrg = useSwitchOrganization();
  const navigate = useNavigate();
  const [, chooseProject] = useProject();
  const { setOpenMobile } = useSidebar();
  if (!auth?.workosEnabled || !auth.organization) return null;
  const orgs = auth.organizations.length > 0 ? auth.organizations : [auth.organization];
  const items = [
    ...orgs.map((o) => ({ value: o.id, label: o.name })),
    { value: "__manage__", label: "+ Manage organizations…" },
  ];

  return (
    <Select
      items={items}
      value={auth.organization.id}
      disabled={switchOrg.isPending}
      onValueChange={(val) => {
        if (!val) return;
        if (val === "__manage__") {
          setOpenMobile(false);
          void navigate({ to: "/org" });
          return;
        }
        if (val !== auth.organization?.id) {
          chooseProject(null);
          switchOrg.mutate(val);
        }
      }}
    >
      <SelectTrigger
        size="sm"
        aria-label="Organization"
        className="h-8 w-full justify-between gap-1.5 border-sidebar-border bg-card/70 px-2.5 font-mono text-[11.5px] text-foreground hover:border-border-strong"
      >
        <span className="flex min-w-0 items-center gap-2">
          <Building2 className="size-3.5 shrink-0 text-primary" />
          <SelectValue className="truncate" />
        </span>
      </SelectTrigger>
      <SelectContent className="min-w-48 font-mono text-[12px]">
        {orgs.map((o) => (
          <SelectItem key={o.id} value={o.id} className="text-[12px]">
            {o.name}
          </SelectItem>
        ))}
        <SelectSeparator />
        <SelectItem value="__manage__" className="text-[12px] text-muted-foreground">
          + Manage organizations…
        </SelectItem>
      </SelectContent>
    </Select>
  );
}

function AccountMenu() {
  const { data: auth } = useAuthMe();
  const navigate = useNavigate();
  const { isMobile, setOpenMobile } = useSidebar();
  if (!auth?.authenticated || !auth.workosEnabled) return null;

  const initials = (auth.user.name || auth.user.email || "U")
    .split(/[\s@._-]+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((p) => p[0]?.toUpperCase() ?? "")
    .join("");

  return (
    <SidebarMenu>
      <SidebarMenuItem>
        <DropdownMenu>
          <DropdownMenuTrigger
            render={
              <SidebarMenuButton
                size="lg"
                className="h-10 rounded-lg px-2 hover:bg-sidebar-accent/80 group-data-[collapsible=icon]:size-8! group-data-[collapsible=icon]:justify-center"
                tooltip={auth.user.email}
              />
            }
          >
            <span className="flex size-6 shrink-0 items-center justify-center rounded-md bg-ember-wash font-mono text-[10px] font-semibold text-primary">
              {initials}
            </span>
            <div className="grid min-w-0 flex-1 text-left leading-tight group-data-[collapsible=icon]:hidden">
              <span className="truncate text-[12px] font-medium text-foreground">{auth.user.name || auth.user.email}</span>
              <span className="truncate font-mono text-[10px] text-muted-foreground">{auth.user.email}</span>
            </div>
            <ChevronsUpDown className="ml-auto size-3.5 shrink-0 text-faint group-data-[collapsible=icon]:hidden" />
          </DropdownMenuTrigger>
          <DropdownMenuContent
            side={isMobile ? "top" : "right"}
            align="end"
            sideOffset={6}
            className="w-56 font-sans"
          >
            <DropdownMenuGroup>
              <DropdownMenuLabel className="font-normal">
                <div className="flex flex-col gap-0.5">
                  <span className="truncate text-[12.5px] font-medium text-foreground">{auth.user.name || "Signed in"}</span>
                  <span className="truncate font-mono text-[10.5px] text-muted-foreground">{auth.user.email}</span>
                </div>
              </DropdownMenuLabel>
            </DropdownMenuGroup>
            <DropdownMenuSeparator />
            <DropdownMenuItem
              onClick={() => {
                setOpenMobile(false);
                void navigate({ to: "/org" });
              }}
            >
              <Building2 className="size-4" />
              Organization & Access
            </DropdownMenuItem>
            <DropdownMenuSeparator />
            <DropdownMenuItem variant="destructive" onClick={() => void signOut()}>
              <LogOut className="size-4" />
              Sign out
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </SidebarMenuItem>
    </SidebarMenu>
  );
}

const ALL_PROJECTS = "__all__";

/** Which project the cockpit shows: one, or every one. */
function ProjectPicker({ compact = false }: { compact?: boolean }) {
  const { data } = useSnapshot();
  const [project, choose] = useProject();
  if (!data) return null;
  const items = [
    { value: ALL_PROJECTS, label: compact ? "all projects" : "every project" },
    ...data.projects.map((p) => ({
      value: p.name,
      label: p.openQuestions > 0 ? `${p.name} (${p.openQuestions} ?)` : p.name,
    })),
  ];

  return (
    <Select
      items={items}
      value={project ?? ALL_PROJECTS}
      onValueChange={(val) => choose(!val || val === ALL_PROJECTS ? null : val)}
    >
      <SelectTrigger
        size="sm"
        aria-label="Project"
        className={
          compact
            ? `h-7 max-w-36 gap-1.5 px-2 font-mono text-[11px] ${project ? "border-primary/50 bg-ember-wash text-foreground" : "border-border bg-card/70 text-foreground"}`
            : `h-8 w-full justify-between gap-1.5 px-2.5 font-mono text-[11.5px] text-foreground hover:border-border-strong ${project ? "border-primary/45 bg-ember-wash/70" : "border-sidebar-border bg-card/70"}`
        }
      >
        <span className="flex min-w-0 items-center gap-1.5">
          <Layers className={`size-3.5 shrink-0 ${project ? "text-primary" : "text-faint"}`} />
          <SelectValue className="truncate" />
        </span>
      </SelectTrigger>
      <SelectContent className="min-w-44 font-mono text-[12px]">
        <SelectItem value={ALL_PROJECTS} className="text-[12px]">
          {compact ? "all projects" : "every project"}
        </SelectItem>
        {data.projects.length > 0 && <SelectSeparator />}
        {data.projects.map((p) => (
          <SelectItem key={p.name} value={p.name} className="text-[12px]">
            {p.name}
            {p.openQuestions > 0 ? ` (${p.openQuestions} ?)` : ""}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}
