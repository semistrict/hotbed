import { useIsMutating, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams, useSearch } from "@tanstack/react-router";
import { ChevronRightIcon } from "lucide-react";
import { useMemo, useRef, useState, type DragEvent } from "react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import {
  api,
  MEMORY_ORDER_FROM_SLUG,
  MemoryMode,
  MemoryScope,
  MemoryState,
  type ListMemoriesResponse,
  type Memory,
  type MemoryOrderSlug,
  type Snapshot,
  type Stamp,
} from "../api";
import { Markdown } from "../components/Markdown";
import { ProjectTag } from "../components/ProjectTag";
import { SessionLink } from "../components/SessionLink";
import { Card, Empty } from "../components/Panel";
import { ago } from "../lib/format";
import {
  computeStartupBudget,
  formatBytesKB,
  MEMORY_TAB_STATES,
  memoryModeLabel,
  mergeLatestMemories,
  type MemoryTab,
} from "../lib/memories";
import { useProject, useScopedSnapshot } from "../lib/project";
import { remoteName } from "../lib/projects";
import { dropAt, placement, reorder, shortHead, who } from "../lib/questions";
import { PageTitle } from "./Nodes";

export interface MemoriesSearch {
  tab?: MemoryTab;
  scope?: "all" | "global" | "project";
  mode?: "all" | "always" | "on-demand";
  sort?: MemoryOrderSlug;
  q?: string;
}

const TABS: { tab: MemoryTab; label: string; empty: string }[] = [
  { tab: "active", label: "Active", empty: "No active memories yet. Agents save learnings with hotbed memory propose, or add one directly." },
  { tab: "history", label: "History", empty: "No archived, superseded, rejected, or withdrawn memories." },
  { tab: "all", label: "All", empty: "No memories yet." },
];

const SORTS: { sort: MemoryOrderSlug; label: string }[] = [
  { sort: "rank", label: "startup order" },
  { sort: "newest", label: "newest" },
  { sort: "oldest", label: "oldest" },
  { sort: "updated", label: "recently changed" },
  { sort: "slug", label: "slug" },
];

const STATE_TAG: Record<MemoryState, { text: string; className: string }> = {
  [MemoryState.UNSPECIFIED]: { text: "✓ active", className: "text-good-ink" },
  [MemoryState.PROPOSED]: { text: "● proposed", className: "text-primary" },
  [MemoryState.APPROVED]: { text: "✓ active", className: "text-good-ink" },
  [MemoryState.REJECTED]: { text: "✕ rejected", className: "text-critical" },
  [MemoryState.SUPERSEDED]: { text: "↻ superseded", className: "text-muted-foreground" },
  [MemoryState.ARCHIVED]: { text: "⊘ archived", className: "text-faint" },
  [MemoryState.WITHDRAWN]: { text: "⊘ withdrawn", className: "text-faint" },
};

const LINK = "font-mono text-[11px] text-muted-foreground underline decoration-border-strong underline-offset-2 hover:text-foreground disabled:opacity-40";
const SORT_ITEMS = SORTS.map((o) => ({ value: o.sort, label: o.label }));
const MODE_ITEMS = [
  { value: String(MemoryMode.ALWAYS), label: "Always-on (inline rule)" },
  { value: String(MemoryMode.ON_DEMAND), label: "On-demand (index + get)" },
];
const ALL_REPOS = "__all_repos__";

const MOVE = ["move-memory"];

function useSettleMemories() {
  const qc = useQueryClient();
  return () => Promise.all([qc.invalidateQueries({ queryKey: ["memories"] }), qc.invalidateQueries({ queryKey: ["memory"] }), qc.invalidateQueries({ queryKey: ["snapshot"] })]);
}

export function MemoriesPage() {
  const { data: s } = useScopedSnapshot();
  if (!s) return null;
  return <MemoriesView s={s} />;
}

export function MemoryDetailPage() {
  const { id } = useParams({ from: "/memories/$id" });
  const navigate = useNavigate();
  const { data: s } = useScopedSnapshot();
  const detail = useQuery({
    queryKey: ["memory", id],
    queryFn: () => api.memory(id),
    refetchInterval: 3000,
  });
  if (!s) return null;
  return (
    <div className="space-y-6">
      <PageTitle
        title={`Memory #${id}`}
        sub="Full details, provenance, and revision history."
        right={
          <Link to="/memories" className="inline-block rounded-lg border border-border px-3 py-1.5 font-mono text-[11.5px] text-muted-foreground hover:border-border-strong hover:text-foreground">
            ← all memories
          </Link>
        }
      />
      {detail.isError && <div className="rounded-xl border border-critical/40 bg-card px-4 py-2.5 font-mono text-[11.5px] text-critical">{String(detail.error)}</div>}
      {detail.data && (
        <MemoryCard
          s={s}
          m={detail.data}
          now={Number(s.now)}
          delay={0}
          mover={null}
          defaultExpanded
          isFull
          onDeleted={() => void navigate({ to: "/memories" })}
        />
      )}
    </div>
  );
}

function MemoriesView({ s }: { s: Snapshot }) {
  const search = useSearch({ from: "/memories" });
  const navigate = useNavigate({ from: "/memories" });
  const [project] = useProject();
  const [creating, setCreating] = useState(false);

  const tab: MemoryTab = search.tab ?? "active";
  const scopeSlug = search.scope ?? "all";
  const modeSlug = search.mode ?? "all";
  const sort: MemoryOrderSlug = search.sort ?? (tab === "active" ? "rank" : "newest");
  const queryText = search.q ?? "";

  const scopeFilter =
    scopeSlug === "global" ? MemoryScope.GLOBAL : scopeSlug === "project" ? MemoryScope.PROJECT : MemoryScope.UNSPECIFIED;
  const modeFilter =
    modeSlug === "always" ? MemoryMode.ALWAYS : modeSlug === "on-demand" ? MemoryMode.ON_DEMAND : MemoryMode.UNSPECIFIED;

  const key = ["memories", project, tab, scopeSlug, modeSlug, sort, queryText] as const;
  const moving = useIsMutating({ mutationKey: MOVE }) > 0;

  const list = useQuery({
    queryKey: key,
    queryFn: () =>
      api.memories({
        project,
        scope: scopeFilter,
        states: MEMORY_TAB_STATES[tab],
        mode: modeFilter,
        query: queryText,
        order: MEMORY_ORDER_FROM_SLUG[sort],
      }),
    refetchInterval: moving ? false : 2500,
    placeholderData: (prev, prevQuery) => (prevQuery?.queryKey[1] === project ? prev : undefined),
  });

  // Fetch active memories for startup budget calculation (global + selected project).
  const budgetQuery = useQuery({
    queryKey: ["memories", project, "budget"],
    queryFn: () => api.memories({ project, states: [MemoryState.APPROVED], order: MEMORY_ORDER_FROM_SLUG.rank }),
    refetchInterval: 5000,
  });

  const seenRef = useRef<{ project: string | null; map: Map<number, Memory> }>({ project, map: new Map() });
  const rawMemories = list.data?.memories;
  const memories = useMemo(() => {
    if (seenRef.current.project !== project) {
      seenRef.current = { project, map: new Map() };
    }
    return rawMemories ? mergeLatestMemories(seenRef.current.map, rawMemories, MEMORY_TAB_STATES[tab]) : [];
  }, [rawMemories, tab, project]);

  const budget = useMemo(() => computeStartupBudget(budgetQuery.data?.memories ?? []), [budgetQuery.data?.memories]);
  const move = useMoveMemory(key);
  const movable = tab === "active" && sort === "rank" && !queryText && modeSlug === "all";
  const now = Number(s.now);

  const counts: Partial<Record<MemoryTab, number>> = {
    active: budgetQuery.data?.total,
  };

  return (
    <div className="space-y-6 [overflow-anchor:auto]">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <PageTitle
          title="Memories"
          sub={`Cross-agent learnings for ${project ? `global + ${project}` : "global + every project"}. Memories are active immediately and returned on every hotbed session start.`}
        />
        <Button
          type="button"
          onClick={() => setCreating((v) => !v)}
          className="text-[12.5px]"
        >
          {creating ? "Cancel" : "+ New memory"}
        </Button>
      </div>

      <BudgetBar budget={budget} project={project} />

      {creating && <NewMemoryForm s={s} defaultProject={project} onDone={() => setCreating(false)} />}

      <div className="rise space-y-2.5">
        <div className="flex flex-wrap items-center gap-2.5">
          <div className="max-w-full overflow-x-auto no-scrollbar">
            <Tabs value={tab} onValueChange={(v) => navigate({ search: { ...search, tab: v as MemoryTab, sort: undefined } })}>
              <TabsList aria-label="Memories" className="border border-border bg-card/70">
                {TABS.map((t) => (
                  <TabsTrigger key={t.tab} value={t.tab} className="px-2.5 text-[12px] sm:px-3 sm:text-[12.5px]">
                    {t.label}
                    {counts[t.tab] ? (
                      <span className="ml-1 font-mono text-[10.5px] text-muted-foreground">{counts[t.tab]}</span>
                    ) : null}
                  </TabsTrigger>
                ))}
              </TabsList>
            </Tabs>
          </div>

          <Input
            type="search"
            value={queryText}
            onChange={(e) => navigate({ search: { ...search, tab, q: e.target.value || undefined } })}
            placeholder="Search slug, summary, body…"
            aria-label="Search memories"
            className="w-full bg-card text-[12px] placeholder:text-faint sm:w-56"
          />

          <div className="flex items-center gap-2 font-mono text-[11px] text-muted-foreground sm:ml-auto">
            <span>sort</span>
            <Select items={SORT_ITEMS} value={sort} onValueChange={(v) => v && navigate({ search: { ...search, tab, sort: v as MemoryOrderSlug } })}>
              <SelectTrigger size="sm" aria-label="Sort memories" className="bg-card font-mono text-[11.5px] text-foreground">
                <SelectValue />
              </SelectTrigger>
              <SelectContent className="font-mono text-[11.5px]">
                {SORTS.map((o) => (
                  <SelectItem key={o.sort} value={o.sort} className="text-[11.5px]">
                    {o.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
        </div>

        <div className="flex flex-wrap items-center gap-2">
          <div className="max-w-full overflow-x-auto no-scrollbar">
            <Tabs value={scopeSlug} onValueChange={(v) => navigate({ search: { ...search, tab, scope: v === "all" ? undefined : (v as "global" | "project") } })}>
              <TabsList className="border border-border bg-card/70 font-mono text-[11px]">
                {(
                  [
                    ["all", project ? `global + ${project}` : "all scopes"],
                    ["global", "global"],
                    ["project", project ?? "projects"],
                  ] as const
                ).map(([val, label]) => (
                  <TabsTrigger key={val} value={val} className="px-2.5 font-mono text-[11px]">
                    {label}
                  </TabsTrigger>
                ))}
              </TabsList>
            </Tabs>
          </div>

          <div className="max-w-full overflow-x-auto no-scrollbar">
            <Tabs value={modeSlug} onValueChange={(v) => navigate({ search: { ...search, tab, mode: v === "all" ? undefined : (v as "always" | "on-demand") } })}>
              <TabsList className="border border-border bg-card/70 font-mono text-[11px]">
                {(
                  [
                    ["all", "all modes"],
                    ["always", "always-on"],
                    ["on-demand", "on-demand"],
                  ] as const
                ).map(([val, label]) => (
                  <TabsTrigger key={val} value={val} className="px-2.5 font-mono text-[11px]">
                    {label}
                  </TabsTrigger>
                ))}
              </TabsList>
            </Tabs>
          </div>
        </div>
      </div>

      {list.isError && (
        <Alert variant="destructive" className="rounded-xl border-critical/40 bg-card px-4 py-2.5 font-mono text-[11.5px]">
          <AlertDescription className="text-[11.5px] text-critical">Could not list memories: {String(list.error)}</AlertDescription>
        </Alert>
      )}
      {move.error && (
        <Alert variant="destructive" className="rounded-xl border-critical/40 bg-card px-4 py-2.5 font-mono text-[11.5px]">
          <AlertDescription className="text-[11.5px] text-critical">Could not reorder: {String(move.error)}</AlertDescription>
        </Alert>
      )}

      {list.data && !list.isPlaceholderData && memories.length === 0 ? (
        <Card>
          <Empty>{TABS.find((t) => t.tab === tab)?.empty}</Empty>
        </Card>
      ) : (
        <MemoryList s={s} memories={memories} now={now} movable={movable} onMove={(id, before) => move.mutate({ id, before })} />
      )}
      {tab === "active" && !movable && memories.length > 1 && (
        <p className="font-mono text-[11px] text-faint">Sort by startup order (with all modes and no search filter) to drag and reorder.</p>
      )}
    </div>
  );
}

function BudgetBar({ budget, project }: { budget: ReturnType<typeof computeStartupBudget>; project: string | null }) {
  const alwaysOver = budget.alwaysCount > budget.alwaysMaxCount || budget.alwaysBytes > budget.alwaysMaxBytes;
  const onDemandOver = budget.onDemandCount > budget.onDemandMaxCount || budget.onDemandBytes > budget.onDemandMaxBytes;
  return (
    <div className="flex flex-wrap items-center gap-x-6 gap-y-2 rounded-xl border border-border bg-card/60 px-4 py-2.5 font-mono text-[11px] text-muted-foreground">
      <span className="text-foreground">Startup recall ({project ? `global + ${project}` : "all"}):</span>
      <span className={alwaysOver ? "text-warning" : ""}>
        Always-on <strong className="text-foreground">{budget.alwaysCount}</strong>/{budget.alwaysMaxCount} · {formatBytesKB(budget.alwaysBytes)}/{formatBytesKB(budget.alwaysMaxBytes)}
      </span>
      <span className={onDemandOver ? "text-warning" : ""}>
        On-demand index <strong className="text-foreground">{budget.onDemandCount}</strong>/{budget.onDemandMaxCount} · {formatBytesKB(budget.onDemandBytes)}/{formatBytesKB(budget.onDemandMaxBytes)}
      </span>
    </div>
  );
}

function useMoveMemory(key: readonly unknown[]) {
  const qc = useQueryClient();
  return useMutation({
    mutationKey: MOVE,
    mutationFn: ({ id, before }: { id: number; before: number | null }) => api.moveMemory(id, before),
    onMutate: async ({ id, before }) => {
      await qc.cancelQueries({ queryKey: key });
      const was = qc.getQueryData<ListMemoriesResponse>(key);
      if (was) qc.setQueryData<ListMemoriesResponse>(key, { ...was, memories: reorder(was.memories, id, before) });
      return { was };
    },
    onError: (_err, _vars, ctx) => {
      if (ctx?.was) qc.setQueryData(key, ctx.was);
    },
    onSettled: () => qc.invalidateQueries({ queryKey: ["memories"] }),
  });
}

function MemoryList({
  s,
  memories,
  now,
  movable,
  onMove,
}: {
  s: Snapshot;
  memories: Memory[];
  now: number;
  movable: boolean;
  onMove: (id: number, before: number | null) => void;
}) {
  const [dragged, setDragged] = useState<number | null>(null);
  const [drop, setDrop] = useState<{ id: number; after: boolean } | null>(null);
  const order = memories.map((m) => m.id);
  const moveTo = (id: number, to: number) => {
    const p = placement(order, id, to);
    if (p) onMove(id, p.before);
  };
  const over = (e: DragEvent, id: number) => {
    if (dragged === null || dragged === id) return;
    e.preventDefault();
    const box = e.currentTarget.getBoundingClientRect();
    setDrop({ id, after: e.clientY > box.top + box.height / 2 });
  };
  const dropped = (e: DragEvent) => {
    e.preventDefault();
    const to = dragged !== null && drop ? dropAt(order, dragged, drop.id, drop.after) : undefined;
    if (dragged !== null && to !== undefined) moveTo(dragged, to);
    setDragged(null);
    setDrop(null);
  };

  return (
    <ol className="space-y-4">
      {memories.map((m, i) => (
        <li
          key={m.id}
          draggable={movable}
          onDragStart={(e) => {
            setDragged(m.id);
            e.dataTransfer.effectAllowed = "move";
          }}
          onDragEnd={() => {
            setDragged(null);
            setDrop(null);
          }}
          onDragOver={(e) => over(e, m.id)}
          onDrop={dropped}
          className={`relative ${dragged === m.id ? "opacity-40" : ""}`}
        >
          {drop?.id === m.id && dragged !== m.id && <span aria-hidden className={`absolute right-2 left-2 h-0.5 rounded-full bg-primary ${drop.after ? "-bottom-2.5" : "-top-2.5"}`} />}
          <MemoryCard
            s={s}
            m={m}
            now={now}
            delay={Math.min(i, 8) * 35}
            mover={
              movable ? (
                <Mover first={i === 0} last={i === memories.length - 1} onUp={() => moveTo(m.id, i - 1)} onDown={() => moveTo(m.id, i + 1)} />
              ) : null
            }
          />
        </li>
      ))}
    </ol>
  );
}

function Mover({ first, last, onUp, onDown }: { first: boolean; last: boolean; onUp: () => void; onDown: () => void }) {
  const btn = "rounded p-1.5 leading-none text-muted-foreground hover:bg-muted hover:text-foreground disabled:opacity-30 disabled:hover:bg-transparent sm:px-1 sm:py-0.5";
  return (
    <div className="flex shrink-0 flex-col items-center gap-0.5 font-mono text-[11px]">
      <button type="button" className={btn} disabled={first} onClick={onUp} aria-label="Move up" title="Move up">
        ▲
      </button>
      <span className="cursor-grab text-faint select-none active:cursor-grabbing" title="Drag to reorder" aria-hidden>
        ⋮⋮
      </span>
      <button type="button" className={btn} disabled={last} onClick={onDown} aria-label="Move down" title="Move down">
        ▼
      </button>
    </div>
  );
}

function MemoryCard({
  s,
  m,
  now,
  delay,
  mover,
  defaultExpanded = false,
  isFull = false,
  onDeleted,
}: {
  s: Snapshot;
  m: Memory;
  now: number;
  delay: number;
  mover: React.ReactNode;
  defaultExpanded?: boolean;
  isFull?: boolean;
  onDeleted?: () => void;
}) {
  const tag = STATE_TAG[m.state] ?? STATE_TAG[MemoryState.APPROVED];
  const [expanded, setExpanded] = useState(defaultExpanded);
  const [editing, setEditing] = useState(false);
  const [confirmRemove, setConfirmRemove] = useState(false);
  const settle = useSettleMemories();

  // ListMemories omits body to stay compact; fetch full detail whenever expanded or editing.
  const needsFull = expanded || editing;
  const detail = useQuery({
    queryKey: ["memory", String(m.id), String(m.revision)],
    queryFn: () => api.memory(String(m.id)),
    enabled: needsFull && !isFull,
  });
  const hasFull = isFull || detail.data !== undefined;
  const full = detail.data ?? m;
  const content = full.content;

  const promote = useMutation({
    mutationFn: () => api.reviseMemory(m.id, { global: true, ifRevision: full.revision }),
    onSettled: settle,
  });
  const archive = useMutation({
    mutationFn: () => api.archiveMemory(m.id, "", full.revision),
    onSettled: settle,
  });
  const restore = useMutation({
    mutationFn: () => api.restoreMemory(m.id, full.revision),
    onSettled: settle,
  });
  const remove = useMutation({
    mutationFn: () => api.deleteMemory(m.id, full.revision),
    onSuccess: () => {
      setConfirmRemove(false);
      onDeleted?.();
    },
    onSettled: settle,
  });
  const toggleMode = useMutation({
    mutationFn: async () => {
      const current = hasFull ? full : await api.memory(String(m.id));
      const c = current.content;
      const nextMode = c?.mode === MemoryMode.ALWAYS ? MemoryMode.ON_DEMAND : MemoryMode.ALWAYS;
      return api.reviseMemory(m.id, {
        content: {
          mode: nextMode,
          ...(nextMode === MemoryMode.ON_DEMAND && !c?.body?.trim() ? { body: c?.summary ?? "" } : {}),
        },
        ifRevision: current.revision,
      });
    },
    onSettled: settle,
  });

  const busy = promote.isPending || archive.isPending || restore.isPending || remove.isPending || toggleMode.isPending;
  const actionError = promote.error ?? archive.error ?? restore.error ?? remove.error ?? toggleMode.error;
  const muted = m.state === MemoryState.ARCHIVED || m.state === MemoryState.WITHDRAWN || m.state === MemoryState.SUPERSEDED;
  const isProjectScoped = m.scope === MemoryScope.PROJECT && m.project !== "";

  return (
    <Card delay={delay} className={muted ? "opacity-75" : ""}>
      <div className="flex gap-2 sm:gap-3">
        {mover}
        <div className="min-w-0 flex-1 space-y-3">
          <header className="flex flex-wrap items-center gap-x-2.5 gap-y-1">
            <Link to="/memories/$id" params={{ id: String(m.id) }} className="break-all font-mono text-[13px] font-semibold text-foreground hover:underline">
              {content?.slug ?? `#${m.id}`}
            </Link>
            <span className="font-mono text-[10.5px] text-faint">#{m.id}</span>
            {m.scope === MemoryScope.GLOBAL || !m.project ? (
              <span className="rounded-md border border-frost/40 bg-frost-wash px-1.5 py-px font-mono text-[10px] text-frost">global</span>
            ) : (
              <ProjectTag name={m.project} />
            )}
            {m.repo && (
              <span className="font-mono text-[10.5px] text-muted-foreground" title={m.repo}>
                {remoteName(m.repo)}
              </span>
            )}
            <span
              className={`rounded-md px-1.5 py-px font-mono text-[10px] ${
                content?.mode === MemoryMode.ALWAYS ? "bg-ember-wash text-primary" : "border border-border text-muted-foreground"
              }`}
            >
              {memoryModeLabel(content?.mode ?? MemoryMode.ON_DEMAND)}
            </span>
            <span className={`font-mono text-[10.5px] ${tag.className}`}>{tag.text}</span>
            {m.supersededBy > 0 && (
              <Link to="/memories/$id" params={{ id: String(m.supersededBy) }} className="font-mono text-[10.5px] text-primary hover:underline">
                → #{m.supersededBy}
              </Link>
            )}
            {m.proposed && (
              <span className="text-[11px] text-muted-foreground sm:ml-auto">
                <StampLine s={s} st={m.proposed} now={now} verb="saved" />
              </span>
            )}
          </header>

          {editing ? (
            hasFull ? (
              <MemoryEditor
                key={`${full.id}:${String(full.revision)}`}
                s={s}
                m={full}
                onDone={() => setEditing(false)}
              />
            ) : (
              <div className="flex items-center gap-3 rounded-lg border border-border bg-muted/30 p-3 font-mono text-[11.5px] text-muted-foreground">
                {detail.isError ? (
                  <span className="text-critical">Could not load memory: {String(detail.error)}</span>
                ) : (
                  <span>loading memory…</span>
                )}
                <button type="button" onClick={() => setEditing(false)} className={LINK}>
                  cancel
                </button>
              </div>
            )
          ) : (
            <>
              <div className="space-y-1.5">
                {content?.title && <div className="text-[14px] font-semibold text-foreground">{content.title}</div>}
                <p className="text-[13.5px] leading-snug text-foreground">{content?.summary}</p>
                {(content?.tags?.length ?? 0) > 0 && (
                  <div className="flex flex-wrap gap-1.5 pt-0.5">
                    {content!.tags.map((t) => (
                      <Badge key={t} variant="secondary" className="h-auto rounded px-1.5 py-px font-mono text-[10px] font-normal text-muted-foreground">
                        #{t}
                      </Badge>
                    ))}
                  </div>
                )}
              </div>

              {full.provenance?.rationale && (
                <div className="rounded-lg border border-border bg-muted/40 px-3 py-2 text-[12px] text-muted-foreground">
                  <span className="font-mono text-[10.5px] text-foreground">Rationale: </span>
                  {full.provenance.rationale}
                </div>
              )}

              <div>
                <button type="button" onClick={() => setExpanded((v) => !v)} className={LINK}>
                  {expanded ? "▾ hide details" : "▸ show details"}
                </button>
                {expanded && (
                  <div className="mt-2 space-y-2">
                    {detail.isLoading ? (
                      <div className="font-mono text-[11px] text-faint">loading body…</div>
                    ) : content?.body ? (
                      <div className="rounded-lg border border-border bg-muted/30 px-3.5 py-2.5">
                        <Markdown className="text-[13px] leading-relaxed text-foreground/90">{content.body}</Markdown>
                      </div>
                    ) : (
                      <div className="font-mono text-[11px] text-faint">No extended body (inline rule only).</div>
                    )}
                    <ProvenanceLine m={full} />
                  </div>
                )}
              </div>
            </>
          )}

          {!editing && (
            <div className="flex flex-wrap items-center gap-2.5 pt-1">
              <button
                type="button"
                disabled={busy}
                onClick={() => {
                  setExpanded(true);
                  setConfirmRemove(false);
                  setEditing(true);
                }}
                className={LINK}
              >
                edit
              </button>

              {m.state === MemoryState.APPROVED && (
                <>
                  {isProjectScoped && (
                    <button
                      type="button"
                      disabled={busy}
                      onClick={() => promote.mutate()}
                      className={LINK}
                      title="Make this memory global across all projects"
                    >
                      {promote.isPending ? "promoting…" : "promote to global"}
                    </button>
                  )}
                  <button type="button" disabled={busy} onClick={() => toggleMode.mutate()} className={LINK}>
                    {toggleMode.isPending
                      ? "switching…"
                      : `switch to ${content?.mode === MemoryMode.ALWAYS ? "on-demand" : "always-on"}`}
                  </button>
                  <button type="button" disabled={busy} onClick={() => archive.mutate()} className={LINK}>
                    {archive.isPending ? "archiving…" : "archive"}
                  </button>
                </>
              )}

              {(m.state === MemoryState.ARCHIVED || m.state === MemoryState.REJECTED || m.state === MemoryState.WITHDRAWN) && (
                <button type="button" disabled={busy} onClick={() => restore.mutate()} className={LINK}>
                  {restore.isPending ? "restoring…" : "↩ restore"}
                </button>
              )}

              {confirmRemove ? (
                <>
                  <Button
                    type="button"
                    variant="destructive"
                    size="sm"
                    disabled={busy}
                    onClick={() => remove.mutate()}
                    className="h-6 border-critical/50 px-2 text-[11.5px] text-critical"
                  >
                    {remove.isPending ? "Removing…" : "Confirm remove"}
                  </Button>
                  <button type="button" disabled={busy} onClick={() => setConfirmRemove(false)} className={LINK}>
                    cancel
                  </button>
                </>
              ) : (
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => setConfirmRemove(true)}
                  className={`${LINK} hover:text-critical`}
                >
                  remove
                </button>
              )}

              {actionError && <span className="font-mono text-[11px] text-critical">{String(actionError)}</span>}
            </div>
          )}

          <MemoryHistory s={s} m={full} now={now} />
        </div>
      </div>
    </Card>
  );
}

function MemoryEditor({
  s,
  m,
  onDone,
}: {
  s: Snapshot;
  m: Memory;
  onDone: () => void;
}) {
  const settle = useSettleMemories();
  const c = m.content;
  const [slug, setSlug] = useState(c?.slug ?? "");
  const [title, setTitle] = useState(c?.title ?? "");
  const [memMode, setMemMode] = useState<MemoryMode>(c?.mode || MemoryMode.ON_DEMAND);
  const [scopeVal, setScopeVal] = useState<string>(m.scope === MemoryScope.GLOBAL || !m.project ? "__global__" : m.project);
  const [repoVal, setRepoVal] = useState<string>(m.repo || ALL_REPOS);
  const [summary, setSummary] = useState(c?.summary ?? "");
  const [body, setBody] = useState(c?.body ?? "");
  const [tags, setTags] = useState((c?.tags ?? []).join(", "));
  const [rationale, setRationale] = useState(m.provenance?.rationale ?? "");
  const [preview, setPreview] = useState(false);

  const selectedProject = scopeVal === "__global__" ? undefined : s.projects.find((p) => p.name === scopeVal);
  const projectRepos = selectedProject?.repos ?? [];

  const maxSummary = memMode === MemoryMode.ALWAYS ? 240 : 280;
  const parsedTags = tags
    .split(",")
    .map((t) => t.trim().toLowerCase())
    .filter(Boolean);

  const handleScopeChange = (next: string) => {
    setScopeVal(next);
    if (next === "__global__") {
      setRepoVal(ALL_REPOS);
      return;
    }
    const nextProj = s.projects.find((p) => p.name === next);
    if (repoVal !== ALL_REPOS && !nextProj?.repos.includes(repoVal)) {
      setRepoVal(ALL_REPOS);
    }
  };

  const save = useMutation({
    mutationFn: () =>
      api.reviseMemory(m.id, {
        content: {
          slug: slug.trim().toLowerCase(),
          title: title.trim(),
          summary: summary.trim(),
          body,
          mode: memMode,
          tags: parsedTags,
        },
        replaceContent: true,
        global: scopeVal === "__global__",
        project: scopeVal === "__global__" ? "" : scopeVal,
        repo: scopeVal === "__global__" ? undefined : repoVal === ALL_REPOS ? "" : repoVal,
        rationale: rationale.trim(),
        ifRevision: m.revision,
      }),
    onSuccess: onDone,
    onSettled: settle,
  });

  const scopeItems = [
    { value: "__global__", label: "Global (all projects)" },
    ...s.projects.map((p) => ({ value: p.name, label: `Project: ${p.name}` })),
  ];
  const repoItems = [
    { value: ALL_REPOS, label: `All repos in ${scopeVal}` },
    ...projectRepos.map((r) => ({ value: r, label: remoteName(r) })),
  ];

  return (
    <form
      className="space-y-2.5 rounded-lg border border-border bg-muted/30 p-3"
      onSubmit={(e) => {
        e.preventDefault();
        if (!save.isPending) save.mutate();
      }}
    >
      <div className={`grid gap-2 ${projectRepos.length > 0 ? "sm:grid-cols-2 lg:grid-cols-4" : "sm:grid-cols-3"}`}>
        <Input value={slug} onChange={(e) => setSlug(e.target.value)} placeholder="slug-kebab-case" className="bg-background/60 font-mono text-[12.5px]" aria-label="Slug" required />
        <Select items={scopeItems} value={scopeVal} onValueChange={(v) => v && handleScopeChange(v)}>
          <SelectTrigger aria-label="Scope" className="w-full bg-background/60 text-[12.5px]">
            <SelectValue />
          </SelectTrigger>
          <SelectContent className="text-[12.5px]">
            {scopeItems.map((item) => (
              <SelectItem key={item.value} value={item.value} className="text-[12.5px]">
                {item.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        {projectRepos.length > 0 && (
          <Select items={repoItems} value={repoVal} onValueChange={(v) => v && setRepoVal(v)}>
            <SelectTrigger aria-label="Repo" className="w-full bg-background/60 font-mono text-[12px]">
              <SelectValue />
            </SelectTrigger>
            <SelectContent className="font-mono text-[12px]">
              {repoItems.map((item) => (
                <SelectItem key={item.value} value={item.value} className="text-[12px]">
                  {item.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        )}
        <Select items={MODE_ITEMS} value={String(memMode)} onValueChange={(v) => v && setMemMode(Number(v) as MemoryMode)}>
          <SelectTrigger aria-label="Delivery mode" className="w-full bg-background/60 text-[12.5px]">
            <SelectValue />
          </SelectTrigger>
          <SelectContent className="text-[12.5px]">
            {MODE_ITEMS.map((item) => (
              <SelectItem key={item.value} value={item.value} className="text-[12.5px]">
                {item.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>

      <Input value={title} onChange={(e) => setTitle(e.target.value)} placeholder="Title (optional)" className="bg-background/60 text-[12.5px]" aria-label="Title" />

      <div className="space-y-1">
        <Textarea
          rows={2}
          value={summary}
          onChange={(e) => setSummary(e.target.value)}
          placeholder={memMode === MemoryMode.ALWAYS ? "Concise rule returned on every session start" : "1-line description + when-to-read trigger"}
          className="min-h-14 bg-background/60 text-[12.5px]"
          aria-label="Summary"
          required
        />
        <div className={`text-right font-mono text-[10.5px] ${Array.from(summary).length > maxSummary ? "text-critical" : "text-faint"}`}>
          {Array.from(summary).length} / {maxSummary} chars
        </div>
      </div>

      <div className="space-y-1">
        <div className="flex items-center justify-between">
          <span className="font-mono text-[10.5px] text-muted-foreground">
            Body (markdown{memMode === MemoryMode.ON_DEMAND ? ", required for on-demand" : ", optional for always-on"})
          </span>
          <button type="button" onClick={() => setPreview((v) => !v)} className={LINK}>
            {preview ? "edit markdown" : "preview markdown"}
          </button>
        </div>
        {preview ? (
          <div className="min-h-24 rounded-lg border border-border bg-background/60 p-3">
            {body.trim() ? <Markdown className="text-[13px]">{body}</Markdown> : <span className="font-mono text-[11px] text-faint">Empty body</span>}
          </div>
        ) : (
          <Textarea
            rows={Math.min(16, Math.max(4, body.split("\n").length + 1))}
            value={body}
            onChange={(e) => setBody(e.target.value)}
            placeholder="Full details, commands, examples…"
            className="bg-background/60 font-mono text-[12px]"
            aria-label="Body"
          />
        )}
      </div>

      <div className="grid gap-2 sm:grid-cols-2">
        <Input value={tags} onChange={(e) => setTags(e.target.value)} placeholder="Tags (comma-separated, optional)" className="bg-background/60 font-mono text-[11.5px]" aria-label="Tags" />
        <Input value={rationale} onChange={(e) => setRationale(e.target.value)} placeholder="Rationale (optional)" className="bg-background/60 text-[12px]" aria-label="Rationale" />
      </div>

      <div className="flex flex-wrap items-center gap-2 pt-1">
        <Button
          type="submit"
          size="sm"
          disabled={save.isPending || !slug.trim() || !summary.trim()}
          className="text-[12.5px]"
        >
          {save.isPending ? "Saving…" : "Save changes"}
        </Button>
        <Button type="button" variant="ghost" size="sm" onClick={onDone} className="text-[12px] text-muted-foreground">
          cancel
        </Button>
        {save.error && <span className="font-mono text-[11px] text-critical">{String(save.error)}</span>}
      </div>
    </form>
  );
}

function NewMemoryForm({ s, defaultProject, onDone }: { s: Snapshot; defaultProject: string | null; onDone: () => void }) {
  const settle = useSettleMemories();
  const [slug, setSlug] = useState("");
  const [title, setTitle] = useState("");
  const [memMode, setMemMode] = useState<MemoryMode>(MemoryMode.ALWAYS);
  const [scopeVal, setScopeVal] = useState<string>(defaultProject ?? "__global__");
  const [repoVal, setRepoVal] = useState<string>(ALL_REPOS);
  const [summary, setSummary] = useState("");
  const [body, setBody] = useState("");
  const [tags, setTags] = useState("");
  const [rationale, setRationale] = useState("");
  const [preview, setPreview] = useState(false);

  const selectedProject = scopeVal === "__global__" ? undefined : s.projects.find((p) => p.name === scopeVal);
  const projectRepos = selectedProject?.repos ?? [];

  const maxSummary = memMode === MemoryMode.ALWAYS ? 240 : 280;
  const scopeItems = [
    { value: "__global__", label: "Global (all projects)" },
    ...s.projects.map((p) => ({ value: p.name, label: `Project: ${p.name}` })),
  ];
  const repoItems = [
    { value: ALL_REPOS, label: `All repos in ${scopeVal}` },
    ...projectRepos.map((r) => ({ value: r, label: remoteName(r) })),
  ];

  const handleScopeChange = (next: string) => {
    setScopeVal(next);
    setRepoVal(ALL_REPOS);
  };

  const createMut = useMutation({
    mutationFn: () =>
      api.proposeMemory({
        content: {
          slug: slug.trim().toLowerCase(),
          title: title.trim(),
          summary: summary.trim(),
          body,
          mode: memMode,
          tags: tags
            .split(",")
            .map((t) => t.trim().toLowerCase())
            .filter(Boolean),
        },
        global: scopeVal === "__global__",
        project: scopeVal === "__global__" ? "" : scopeVal,
        repo: scopeVal === "__global__" || repoVal === ALL_REPOS ? "" : repoVal,
        rationale: rationale.trim(),
      }),
    onSuccess: onDone,
    onSettled: settle,
  });

  return (
    <Card>
      <form
        className="space-y-3"
        onSubmit={(e) => {
          e.preventDefault();
          if (!createMut.isPending) createMut.mutate();
        }}
      >
        <div className="text-[13.5px] font-semibold text-foreground">New memory</div>
        <div className={`grid gap-2 ${projectRepos.length > 0 ? "sm:grid-cols-2 lg:grid-cols-4" : "sm:grid-cols-3"}`}>
          <Input value={slug} onChange={(e) => setSlug(e.target.value)} placeholder="slug-kebab-case" className="bg-background/60 font-mono text-[12.5px]" required autoFocus />
          <Select items={scopeItems} value={scopeVal} onValueChange={(v) => v && handleScopeChange(v)}>
            <SelectTrigger aria-label="Scope" className="w-full bg-background/60 text-[12.5px]">
              <SelectValue />
            </SelectTrigger>
            <SelectContent className="text-[12.5px]">
              {scopeItems.map((item) => (
                <SelectItem key={item.value} value={item.value} className="text-[12.5px]">
                  {item.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          {projectRepos.length > 0 && (
            <Select items={repoItems} value={repoVal} onValueChange={(v) => v && setRepoVal(v)}>
              <SelectTrigger aria-label="Repo" className="w-full bg-background/60 font-mono text-[12px]">
                <SelectValue />
              </SelectTrigger>
              <SelectContent className="font-mono text-[12px]">
                {repoItems.map((item) => (
                  <SelectItem key={item.value} value={item.value} className="text-[12px]">
                    {item.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          )}
          <Select items={MODE_ITEMS} value={String(memMode)} onValueChange={(v) => v && setMemMode(Number(v) as MemoryMode)}>
            <SelectTrigger aria-label="Delivery mode" className="w-full bg-background/60 text-[12.5px]">
              <SelectValue />
            </SelectTrigger>
            <SelectContent className="text-[12.5px]">
              {MODE_ITEMS.map((item) => (
                <SelectItem key={item.value} value={item.value} className="text-[12.5px]">
                  {item.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <Input value={title} onChange={(e) => setTitle(e.target.value)} placeholder="Title (optional)" className="bg-background/60 text-[12.5px]" />
        <div className="space-y-1">
          <Textarea
            rows={2}
            value={summary}
            onChange={(e) => setSummary(e.target.value)}
            placeholder={memMode === MemoryMode.ALWAYS ? "Concise rule shown on every session start" : "1-line summary + when-to-read trigger"}
            className="min-h-14 bg-background/60 text-[12.5px]"
            required
          />
          <div className={`text-right font-mono text-[10.5px] ${Array.from(summary).length > maxSummary ? "text-critical" : "text-faint"}`}>
            {Array.from(summary).length} / {maxSummary} chars
          </div>
        </div>
        <div className="space-y-1">
          <div className="flex items-center justify-between">
            <span className="font-mono text-[10.5px] text-muted-foreground">
              Body (markdown{memMode === MemoryMode.ON_DEMAND ? ", required for on-demand" : ", optional for always-on"})
            </span>
            <button type="button" onClick={() => setPreview((v) => !v)} className={LINK}>
              {preview ? "edit markdown" : "preview markdown"}
            </button>
          </div>
          {preview ? (
            <div className="min-h-24 rounded-lg border border-border bg-background/60 p-3">
              {body.trim() ? <Markdown className="text-[13px]">{body}</Markdown> : <span className="font-mono text-[11px] text-faint">Empty body</span>}
            </div>
          ) : (
            <Textarea
              rows={4}
              value={body}
              onChange={(e) => setBody(e.target.value)}
              placeholder={memMode === MemoryMode.ON_DEMAND ? "Full markdown details (required for on-demand)" : "Optional rationale / examples (markdown)"}
              className="bg-background/60 font-mono text-[12px]"
            />
          )}
        </div>
        <div className="grid gap-2 sm:grid-cols-2">
          <Input value={tags} onChange={(e) => setTags(e.target.value)} placeholder="Tags (comma-separated, optional)" className="bg-background/60 font-mono text-[11.5px]" />
          <Input value={rationale} onChange={(e) => setRationale(e.target.value)} placeholder="Rationale (optional)" className="bg-background/60 text-[12px]" />
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Button
            type="submit"
            size="sm"
            disabled={createMut.isPending || !slug.trim() || !summary.trim()}
            className="text-[12.5px]"
          >
            {createMut.isPending ? "Saving…" : "Create memory"}
          </Button>
          <Button type="button" variant="ghost" size="sm" onClick={onDone} className="text-[12px] text-muted-foreground">
            cancel
          </Button>
          {createMut.error && <span className="font-mono text-[11px] text-critical">{String(createMut.error)}</span>}
        </div>
      </form>
    </Card>
  );
}

function ProvenanceLine({ m }: { m: Memory }) {
  const p = m.provenance;
  if (!p) return null;
  const f = p.from;
  if (!f && !p.t3codeThreadId && !p.model) return null;
  const repo = f ? (f.remote || f.dir).replace(/\.git$/, "").split(/[/:]/).pop() : "";
  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1 font-mono text-[10.5px] text-muted-foreground">
      {f && (
        <span>
          from {repo}
          {f.branch && <> @ {f.branch}</>}
          {f.head && <span className="text-faint"> {shortHead(f.head)}</span>}
          {f.dirtyFiles > 0 && <span className="text-warning"> · {f.dirtyFiles} changed</span>}
        </span>
      )}
      {p.model && <span>model {p.model}</span>}
      {p.t3codeThreadId && <span className="break-all text-faint">thread {p.t3codeThreadId}</span>}
    </div>
  );
}

function StampLine({ s, st, now, verb }: { s: Snapshot; st: Stamp; now: number; verb: string }) {
  const listed = st.session !== "" && s.sessions.some((x) => x.id === st.session);
  return (
    <span title={new Date(Number(st.at)).toLocaleString()}>
      {verb} by {st.session === "" ? <span className="text-foreground">you</span> : listed ? <SessionLink id={st.session} /> : <span className="font-mono text-[11px]">{st.session}</span>}
      {st.host && <span className="text-faint"> on {st.host}</span>}, {ago(st.at, now)}
    </span>
  );
}

function MemoryHistory({ s, m, now }: { s: Snapshot; m: Memory; now: number }) {
  const [open, setOpen] = useState(false);
  const hist = useQuery({
    queryKey: ["memory-history", m.id, String(m.revision)],
    queryFn: async () => (await api.memory(String(m.id))).history,
    enabled: open && m.history.length === 0,
  });
  const items = m.history.length > 0 ? m.history : hist.data;

  return (
    <Collapsible open={open} onOpenChange={setOpen}>
      <CollapsibleTrigger className="group inline-flex cursor-pointer items-center gap-1 font-mono text-[11px] text-faint hover:text-foreground">
        <ChevronRightIcon className="size-3 transition-transform group-data-panel-open:rotate-90" />
        <span>
          history (rev {String(m.revision)}){m.edited && <span> · edited by {who(m.edited)}</span>}
        </span>
      </CollapsibleTrigger>
      <CollapsibleContent>
        {hist.isError && <div className="mt-2 font-mono text-[11px] text-critical">{String(hist.error)}</div>}
        {items && (
          <ol className="mt-2 space-y-1.5 border-l border-border pl-3">
            {items.map((c) => (
              <li key={String(c.seq)} className="text-[11.5px] text-muted-foreground">
                <span className="font-mono text-[11px] text-foreground">{c.what}</span> {c.by && <StampLine s={s} st={c.by} now={now} verb="" />}
                {c.by?.reason && <span>: {c.by.reason}</span>}
              </li>
            ))}
          </ol>
        )}
      </CollapsibleContent>
    </Collapsible>
  );
}
