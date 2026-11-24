import { useIsMutating, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams, useSearch } from "@tanstack/react-router";
import { useMemo, useRef, useState, type DragEvent } from "react";
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
import { Card, Empty } from "../components/ui/Card";
import { ago } from "../lib/format";
import {
  computeLineDiff,
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
  { tab: "proposals", label: "Proposals", empty: "No proposed memories waiting for you. An agent proposes with hotbed memory propose." },
  { tab: "approved", label: "Approved", empty: "No approved memories yet. Approve a proposal or add one directly." },
  { tab: "history", label: "History", empty: "No rejected, superseded, archived, or withdrawn memories." },
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
  [MemoryState.UNSPECIFIED]: { text: "● proposed", className: "text-primary" },
  [MemoryState.PROPOSED]: { text: "● proposed", className: "text-primary" },
  [MemoryState.APPROVED]: { text: "✓ approved", className: "text-good-ink" },
  [MemoryState.REJECTED]: { text: "✕ rejected", className: "text-critical" },
  [MemoryState.SUPERSEDED]: { text: "↻ superseded", className: "text-muted-foreground" },
  [MemoryState.ARCHIVED]: { text: "⊘ archived", className: "text-faint" },
  [MemoryState.WITHDRAWN]: { text: "⊘ withdrawn", className: "text-faint" },
};

const LINK = "font-mono text-[11px] text-muted-foreground underline decoration-border-strong underline-offset-2 hover:text-foreground disabled:opacity-40";
const FIELD = "w-full rounded-lg border border-border bg-background/60 px-3 py-2 text-[12.5px] placeholder:text-faint focus:border-primary/50 focus:ring-2 focus:ring-primary/20 focus:outline-none";

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
  const { data: s } = useScopedSnapshot();
  const detail = useQuery({
    queryKey: ["memory", id],
    queryFn: () => api.memory(id),
    refetchInterval: 3000,
  });
  if (!s) return null;
  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <PageTitle title={`Memory #${id}`} sub="Full details, provenance, and revision history." />
        <Link to="/memories" className="rounded-lg border border-border px-3 py-1.5 font-mono text-[11.5px] text-muted-foreground hover:border-border-strong hover:text-foreground">
          ← all memories
        </Link>
      </div>
      {detail.isError && <div className="rounded-xl border border-critical/40 bg-card px-4 py-2.5 font-mono text-[11.5px] text-critical">{String(detail.error)}</div>}
      {detail.data && <MemoryCard s={s} m={detail.data} now={Number(s.now)} delay={0} mover={null} defaultExpanded />}
    </div>
  );
}

function MemoriesView({ s }: { s: Snapshot }) {
  const search = useSearch({ from: "/memories" });
  const navigate = useNavigate({ from: "/memories" });
  const [project] = useProject();
  const [creating, setCreating] = useState(false);

  const tab: MemoryTab = search.tab ?? (s.pendingMemories > 0 ? "proposals" : "approved");
  const scopeSlug = search.scope ?? "all";
  const modeSlug = search.mode ?? "all";
  const sort: MemoryOrderSlug = search.sort ?? (tab === "approved" ? "rank" : "newest");
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

  // Fetch approved memories for startup budget calculation (global + selected project).
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
  const movable = tab === "approved" && sort === "rank" && !queryText && modeSlug === "all";
  const now = Number(s.now);

  const counts: Partial<Record<MemoryTab, number>> = {
    proposals: s.pendingMemories,
    approved: budgetQuery.data?.total,
  };

  return (
    <div className="space-y-6 [overflow-anchor:auto]">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <PageTitle
          title="Memories"
          sub={`Cross-agent learnings for ${project ? `global + ${project}` : "global + every project"}. Approved always-on rules and on-demand indexes are returned on every hotbed session start.`}
        />
        <button
          type="button"
          onClick={() => setCreating((v) => !v)}
          className="rounded-lg bg-primary px-3.5 py-2 text-[12.5px] font-medium text-primary-foreground transition-opacity hover:opacity-90"
        >
          {creating ? "Cancel" : "+ New memory"}
        </button>
      </div>

      <BudgetBar budget={budget} project={project} />

      {creating && <NewMemoryForm s={s} defaultProject={project} onDone={() => setCreating(false)} />}

      <div className="rise flex flex-wrap items-center gap-3">
        <div role="tablist" aria-label="Memories" className="flex rounded-lg border border-border bg-card/70 p-0.5">
          {TABS.map((t) => (
            <button
              key={t.tab}
              type="button"
              role="tab"
              aria-selected={tab === t.tab}
              onClick={() => navigate({ search: { ...search, tab: t.tab, sort: undefined } })}
              className={`rounded-md px-3 py-1.5 text-[12.5px] transition-colors ${tab === t.tab ? "bg-muted text-foreground" : "text-muted-foreground hover:text-foreground"}`}
            >
              {t.label}
              {counts[t.tab] ? (
                <span className={`ml-1.5 font-mono text-[10.5px] ${t.tab === "proposals" ? "text-primary" : "text-muted-foreground"}`}>{counts[t.tab]}</span>
              ) : null}
            </button>
          ))}
        </div>

        <div className="flex rounded-lg border border-border bg-card/70 p-0.5 font-mono text-[11px]">
          {(
            [
              ["all", project ? `global + ${project}` : "all scopes"],
              ["global", "global"],
              ["project", project ?? "projects"],
            ] as const
          ).map(([val, label]) => (
            <button
              key={val}
              type="button"
              onClick={() => navigate({ search: { ...search, tab, scope: val === "all" ? undefined : val } })}
              className={`rounded-md px-2.5 py-1 transition-colors ${scopeSlug === val ? "bg-muted text-foreground" : "text-muted-foreground hover:text-foreground"}`}
            >
              {label}
            </button>
          ))}
        </div>

        <div className="flex rounded-lg border border-border bg-card/70 p-0.5 font-mono text-[11px]">
          {(
            [
              ["all", "all modes"],
              ["always", "always-on"],
              ["on-demand", "on-demand"],
            ] as const
          ).map(([val, label]) => (
            <button
              key={val}
              type="button"
              onClick={() => navigate({ search: { ...search, tab, mode: val === "all" ? undefined : val } })}
              className={`rounded-md px-2.5 py-1 transition-colors ${modeSlug === val ? "bg-muted text-foreground" : "text-muted-foreground hover:text-foreground"}`}
            >
              {label}
            </button>
          ))}
        </div>

        <input
          type="search"
          value={queryText}
          onChange={(e) => navigate({ search: { ...search, tab, q: e.target.value || undefined } })}
          placeholder="Search slug, summary, body…"
          aria-label="Search memories"
          className="w-56 rounded-lg border border-border bg-card px-2.5 py-1 text-[12px] text-foreground placeholder:text-faint focus:border-primary/50 focus:outline-none"
        />

        <label className="ml-auto flex items-center gap-2 font-mono text-[11px] text-muted-foreground">
          sort
          <select
            value={sort}
            onChange={(e) => navigate({ search: { ...search, tab, sort: e.target.value as MemoryOrderSlug } })}
            className="rounded-md border border-border bg-card px-2 py-1 text-[11.5px] text-foreground focus:ring-2 focus:ring-primary/40 focus:outline-none"
          >
            {SORTS.map((o) => (
              <option key={o.sort} value={o.sort}>
                {o.label}
              </option>
            ))}
          </select>
        </label>
      </div>

      {list.isError && <div className="rounded-xl border border-critical/40 bg-card px-4 py-2.5 font-mono text-[11.5px] text-critical">Could not list memories: {String(list.error)}</div>}
      {move.error && <div className="rounded-xl border border-critical/40 bg-card px-4 py-2.5 font-mono text-[11.5px] text-critical">Could not reorder: {String(move.error)}</div>}

      {list.data && !list.isPlaceholderData && memories.length === 0 ? (
        <Card>
          <Empty>{TABS.find((t) => t.tab === tab)?.empty}</Empty>
        </Card>
      ) : (
        <MemoryList s={s} memories={memories} now={now} movable={movable} onMove={(id, before) => move.mutate({ id, before })} />
      )}
      {tab === "approved" && !movable && memories.length > 1 && (
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
  const btn = "rounded px-1 leading-none text-muted-foreground hover:bg-muted hover:text-foreground disabled:opacity-30 disabled:hover:bg-transparent";
  return (
    <div className="flex flex-col items-center gap-0.5 font-mono text-[11px]">
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
}: {
  s: Snapshot;
  m: Memory;
  now: number;
  delay: number;
  mover: React.ReactNode;
  defaultExpanded?: boolean;
}) {
  const tag = STATE_TAG[m.state] ?? STATE_TAG[MemoryState.PROPOSED];
  const [expanded, setExpanded] = useState(defaultExpanded || m.state === MemoryState.PROPOSED);
  const [editing, setEditing] = useState<"approve" | "revise" | null>(null);
  const [rejecting, setRejecting] = useState(false);
  const [reason, setReason] = useState("");
  const settle = useSettleMemories();

  // ListMemories omits body to stay compact; fetch full detail when expanded or editing.
  const needsFull = expanded || editing !== null || m.supersedesIds.length > 0;
  const detail = useQuery({
    queryKey: ["memory", String(m.id), String(m.revision)],
    queryFn: () => api.memory(String(m.id)),
    enabled: needsFull && !m.content?.body && m.history.length === 0,
  });
  const full = detail.data ?? m;
  const content = full.content;

  const approve = useMutation({
    mutationFn: () => api.approveMemory(m.id, { ifRevision: full.revision }),
    onSettled: settle,
  });
  const reject = useMutation({
    mutationFn: () => api.rejectMemory(m.id, reason, full.revision),
    onSuccess: () => {
      setRejecting(false);
      setReason("");
    },
    onSettled: settle,
  });
  const archive = useMutation({
    mutationFn: () => api.archiveMemory(m.id, "", full.revision),
    onSettled: settle,
  });
  const restore = useMutation({
    mutationFn: (toProposed: boolean) => api.restoreMemory(m.id, toProposed, full.revision),
    onSettled: settle,
  });
  const toggleMode = useMutation({
    mutationFn: () => {
      const nextMode = content?.mode === MemoryMode.ALWAYS ? MemoryMode.ON_DEMAND : MemoryMode.ALWAYS;
      return api.reviseMemory(m.id, {
        content: {
          slug: content?.slug ?? "",
          title: content?.title ?? "",
          summary: content?.summary ?? "",
          body: content?.body || (nextMode === MemoryMode.ON_DEMAND ? content?.summary ?? "" : ""),
          mode: nextMode,
          tags: content?.tags ?? [],
        },
        ifRevision: full.revision,
      });
    },
    onSettled: settle,
  });

  const busy = approve.isPending || reject.isPending || archive.isPending || restore.isPending || toggleMode.isPending;
  const actionError = approve.error ?? reject.error ?? archive.error ?? restore.error ?? toggleMode.error;
  const muted = m.state === MemoryState.ARCHIVED || m.state === MemoryState.WITHDRAWN || m.state === MemoryState.SUPERSEDED;

  return (
    <Card delay={delay} className={muted ? "opacity-75" : ""}>
      <div className="flex gap-3">
        {mover}
        <div className="min-w-0 flex-1 space-y-3">
          <header className="flex flex-wrap items-center gap-x-2.5 gap-y-1">
            <Link to="/memories/$id" params={{ id: String(m.id) }} className="font-mono text-[13px] font-semibold text-foreground hover:underline">
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
              <span className="ml-auto text-[11px] text-muted-foreground">
                <StampLine s={s} st={m.proposed} now={now} verb="proposed" />
              </span>
            )}
          </header>

          {editing ? (
            <MemoryEditor
              s={s}
              m={full}
              mode={editing}
              onDone={() => setEditing(null)}
            />
          ) : (
            <>
              <div className="space-y-1.5">
                {content?.title && <div className="text-[14px] font-semibold text-foreground">{content.title}</div>}
                <p className="text-[13.5px] leading-snug text-foreground">{content?.summary}</p>
                {(content?.tags?.length ?? 0) > 0 && (
                  <div className="flex flex-wrap gap-1.5 pt-0.5">
                    {content!.tags.map((t) => (
                      <span key={t} className="rounded bg-muted px-1.5 py-px font-mono text-[10px] text-muted-foreground">
                        #{t}
                      </span>
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

              {m.supersedesIds.length > 0 && m.state === MemoryState.PROPOSED && (
                <SupersedesDiff proposal={full} />
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
            <div className="flex flex-wrap items-center gap-2 pt-1">
              {m.state === MemoryState.PROPOSED && (
                <>
                  <button
                    type="button"
                    disabled={busy}
                    onClick={() => approve.mutate()}
                    className="rounded-lg bg-primary px-3.5 py-1.5 text-[12.5px] font-medium text-primary-foreground transition-opacity hover:opacity-90 disabled:opacity-40"
                  >
                    {approve.isPending ? "Approving…" : m.supersedesIds.length > 0 ? "Approve & supersede" : "Approve"}
                  </button>
                  <button
                    type="button"
                    disabled={busy}
                    onClick={() => {
                      setExpanded(true);
                      setEditing("approve");
                    }}
                    className="rounded-lg border border-border-strong px-3 py-1.5 text-[12.5px] text-foreground hover:bg-muted disabled:opacity-40"
                  >
                    Edit & approve
                  </button>
                  {rejecting ? (
                    <>
                      <button
                        type="button"
                        disabled={busy}
                        onClick={() => reject.mutate()}
                        className="rounded-lg border border-critical/50 px-3 py-1.5 text-[12.5px] text-critical hover:bg-muted disabled:opacity-40"
                      >
                        {reject.isPending ? "Rejecting…" : "Confirm reject"}
                      </button>
                      <input
                        value={reason}
                        onChange={(e) => setReason(e.target.value)}
                        placeholder="Why not (optional, sent via poll)"
                        className={`${FIELD} max-w-xs py-1.5`}
                        aria-label="Rejection reason"
                        autoFocus
                      />
                      <button type="button" onClick={() => setRejecting(false)} className="px-2 py-1.5 text-[12px] text-muted-foreground hover:text-foreground">
                        cancel
                      </button>
                    </>
                  ) : (
                    <button
                      type="button"
                      disabled={busy}
                      onClick={() => setRejecting(true)}
                      className="rounded-lg px-3 py-1.5 text-[12.5px] text-muted-foreground hover:bg-muted hover:text-foreground"
                    >
                      Reject…
                    </button>
                  )}
                </>
              )}

              {m.state === MemoryState.APPROVED && (
                <>
                  <button
                    type="button"
                    disabled={busy}
                    onClick={() => {
                      setExpanded(true);
                      setEditing("revise");
                    }}
                    className={LINK}
                  >
                    edit
                  </button>
                  <button type="button" disabled={busy} onClick={() => toggleMode.mutate()} className={LINK}>
                    switch to {content?.mode === MemoryMode.ALWAYS ? "on-demand" : "always-on"}
                  </button>
                  <button type="button" disabled={busy} onClick={() => archive.mutate()} className={LINK}>
                    archive
                  </button>
                </>
              )}

              {(m.state === MemoryState.ARCHIVED || m.state === MemoryState.REJECTED || m.state === MemoryState.WITHDRAWN) && (
                <>
                  <button type="button" disabled={busy} onClick={() => restore.mutate(false)} className={LINK}>
                    ↩ restore as approved
                  </button>
                  <button type="button" disabled={busy} onClick={() => restore.mutate(true)} className={LINK}>
                    restore to proposals
                  </button>
                </>
              )}

              {actionError && <span className="font-mono text-[11px] text-critical">{String(actionError)}</span>}
            </div>
          )}

          {m.decided && m.state === MemoryState.REJECTED && m.decided.reason && (
            <div className="text-[12px] text-muted-foreground">
              Rejected: <span className="text-foreground">{m.decided.reason}</span>
            </div>
          )}

          <MemoryHistory s={s} m={full} now={now} />
        </div>
      </div>
    </Card>
  );
}

function SupersedesDiff({ proposal }: { proposal: Memory }) {
  const targetsQuery = useQuery({
    queryKey: ["memory-supersedes", proposal.id, proposal.supersedesIds.join(",")],
    queryFn: () => Promise.all(proposal.supersedesIds.map((id) => api.memory(String(id)))),
  });

  if (targetsQuery.isLoading) {
    return <div className="font-mono text-[11px] text-faint">loading diff against {proposal.supersedesIds.map((i) => `#${i}`).join(", ")}…</div>;
  }
  if (targetsQuery.isError || !targetsQuery.data) {
    return null;
  }

  const targets = targetsQuery.data;
  const stale = targets.filter((t) => t.state !== MemoryState.APPROVED);
  const prevSummary = targets.map((t) => t.content?.summary ?? "").join("\n");
  const prevBody = targets.map((t) => t.content?.body ?? "").join("\n---\n");
  const summaryDiff = computeLineDiff(prevSummary, proposal.content?.summary ?? "");
  const bodyDiff = computeLineDiff(prevBody, proposal.content?.body ?? "");

  return (
    <div className="space-y-2 rounded-lg border border-border bg-muted/40 p-3">
      <div className="flex flex-wrap items-center gap-2 font-mono text-[11px] text-muted-foreground">
        <span>
          Supersedes {targets.map((t) => `#${t.id} (${t.content?.slug})`).join(", ")}
        </span>
        {stale.length > 0 && (
          <span className="text-warning">
            ⚠ {stale.map((t) => `#${t.id}`).join(", ")} is no longer approved
          </span>
        )}
      </div>
      <div className="space-y-1.5 font-mono text-[11px]">
        <div className="text-[10px] uppercase tracking-wider text-faint">Summary diff</div>
        <DiffBlock lines={summaryDiff} />
        {(prevBody || proposal.content?.body) && (
          <>
            <div className="pt-1 text-[10px] uppercase tracking-wider text-faint">Body diff</div>
            <DiffBlock lines={bodyDiff} />
          </>
        )}
      </div>
    </div>
  );
}

function DiffBlock({ lines }: { lines: ReturnType<typeof computeLineDiff> }) {
  return (
    <pre className="max-h-64 overflow-auto rounded-md border border-border bg-background/70 p-2 leading-relaxed whitespace-pre-wrap">
      {lines.map((l, idx) => (
        <div
          key={idx}
          className={
            l.kind === "add"
              ? "bg-good/10 text-good-ink"
              : l.kind === "del"
                ? "bg-[color-mix(in_oklab,var(--critical)_12%,transparent)] text-critical"
                : "text-muted-foreground"
          }
        >
          {l.kind === "add" ? "+ " : l.kind === "del" ? "- " : "  "}
          {l.text}
        </div>
      ))}
    </pre>
  );
}

function MemoryEditor({
  s,
  m,
  mode,
  onDone,
}: {
  s: Snapshot;
  m: Memory;
  mode: "approve" | "revise";
  onDone: () => void;
}) {
  const settle = useSettleMemories();
  const c = m.content;
  const [slug, setSlug] = useState(c?.slug ?? "");
  const [title, setTitle] = useState(c?.title ?? "");
  const [memMode, setMemMode] = useState<MemoryMode>(c?.mode || MemoryMode.ON_DEMAND);
  const [scopeVal, setScopeVal] = useState<string>(m.scope === MemoryScope.GLOBAL || !m.project ? "__global__" : m.project);
  const [summary, setSummary] = useState(c?.summary ?? "");
  const [body, setBody] = useState(c?.body ?? "");
  const [tags, setTags] = useState((c?.tags ?? []).join(", "));
  const [preview, setPreview] = useState(false);

  const maxSummary = memMode === MemoryMode.ALWAYS ? 240 : 280;
  const parsedTags = tags
    .split(",")
    .map((t) => t.trim().toLowerCase())
    .filter(Boolean);

  const save = useMutation({
    mutationFn: () => {
      const payload = {
        content: {
          slug: slug.trim().toLowerCase(),
          title: title.trim(),
          summary: summary.trim(),
          body,
          mode: memMode,
          tags: parsedTags,
        },
        global: scopeVal === "__global__",
        project: scopeVal === "__global__" ? "" : scopeVal,
        ifRevision: m.revision,
      };
      return mode === "approve" ? api.approveMemory(m.id, payload) : api.reviseMemory(m.id, payload);
    },
    onSuccess: onDone,
    onSettled: settle,
  });

  return (
    <form
      className="space-y-2.5 rounded-lg border border-border bg-muted/30 p-3"
      onSubmit={(e) => {
        e.preventDefault();
        if (!save.isPending) save.mutate();
      }}
    >
      <div className="grid gap-2 sm:grid-cols-3">
        <input value={slug} onChange={(e) => setSlug(e.target.value)} placeholder="slug-kebab-case" className={`${FIELD} font-mono`} aria-label="Slug" required />
        <select value={scopeVal} onChange={(e) => setScopeVal(e.target.value)} className={FIELD} aria-label="Scope">
          <option value="__global__">Global (all projects)</option>
          {s.projects.map((p) => (
            <option key={p.name} value={p.name}>
              Project: {p.name}
            </option>
          ))}
        </select>
        <select
          value={memMode}
          onChange={(e) => setMemMode(Number(e.target.value) as MemoryMode)}
          className={FIELD}
          aria-label="Delivery mode"
        >
          <option value={MemoryMode.ALWAYS}>Always-on (inline rule)</option>
          <option value={MemoryMode.ON_DEMAND}>On-demand (index + get)</option>
        </select>
      </div>

      <input value={title} onChange={(e) => setTitle(e.target.value)} placeholder="Title (optional)" className={FIELD} aria-label="Title" />

      <div className="space-y-1">
        <textarea
          rows={2}
          value={summary}
          onChange={(e) => setSummary(e.target.value)}
          placeholder={memMode === MemoryMode.ALWAYS ? "Concise rule returned on every session start" : "1-line description + when-to-read trigger"}
          className={FIELD}
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
          <textarea
            rows={Math.min(16, Math.max(4, body.split("\n").length + 1))}
            value={body}
            onChange={(e) => setBody(e.target.value)}
            placeholder="Full details, commands, examples…"
            className={`${FIELD} font-mono text-[12px]`}
            aria-label="Body"
          />
        )}
      </div>

      <input value={tags} onChange={(e) => setTags(e.target.value)} placeholder="Tags (comma-separated, optional)" className={`${FIELD} font-mono text-[11.5px]`} aria-label="Tags" />

      <div className="flex flex-wrap items-center gap-2 pt-1">
        <button
          type="submit"
          disabled={save.isPending || !slug.trim() || !summary.trim()}
          className="rounded-lg bg-primary px-3.5 py-1.5 text-[12.5px] font-medium text-primary-foreground transition-opacity hover:opacity-90 disabled:opacity-40"
        >
          {save.isPending ? "Saving…" : mode === "approve" ? "Save & approve" : "Save changes"}
        </button>
        <button type="button" onClick={onDone} className="px-2 py-1.5 text-[12px] text-muted-foreground hover:text-foreground">
          cancel
        </button>
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
  const [summary, setSummary] = useState("");
  const [body, setBody] = useState("");
  const [tags, setTags] = useState("");

  const maxSummary = memMode === MemoryMode.ALWAYS ? 240 : 280;
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
        approve: true,
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
        <div className="text-[13.5px] font-semibold text-foreground">New approved memory</div>
        <div className="grid gap-2 sm:grid-cols-3">
          <input value={slug} onChange={(e) => setSlug(e.target.value)} placeholder="slug-kebab-case" className={`${FIELD} font-mono`} required autoFocus />
          <select value={scopeVal} onChange={(e) => setScopeVal(e.target.value)} className={FIELD}>
            <option value="__global__">Global (all projects)</option>
            {s.projects.map((p) => (
              <option key={p.name} value={p.name}>
                Project: {p.name}
              </option>
            ))}
          </select>
          <select value={memMode} onChange={(e) => setMemMode(Number(e.target.value) as MemoryMode)} className={FIELD}>
            <option value={MemoryMode.ALWAYS}>Always-on (inline rule)</option>
            <option value={MemoryMode.ON_DEMAND}>On-demand (index + get)</option>
          </select>
        </div>
        <input value={title} onChange={(e) => setTitle(e.target.value)} placeholder="Title (optional)" className={FIELD} />
        <div className="space-y-1">
          <textarea
            rows={2}
            value={summary}
            onChange={(e) => setSummary(e.target.value)}
            placeholder={memMode === MemoryMode.ALWAYS ? "Concise rule shown on every session start" : "1-line summary + when-to-read trigger"}
            className={FIELD}
            required
          />
          <div className={`text-right font-mono text-[10.5px] ${Array.from(summary).length > maxSummary ? "text-critical" : "text-faint"}`}>
            {Array.from(summary).length} / {maxSummary} chars
          </div>
        </div>
        <textarea
          rows={4}
          value={body}
          onChange={(e) => setBody(e.target.value)}
          placeholder={memMode === MemoryMode.ON_DEMAND ? "Full markdown details (required for on-demand)" : "Optional rationale / examples (markdown)"}
          className={`${FIELD} font-mono text-[12px]`}
        />
        <input value={tags} onChange={(e) => setTags(e.target.value)} placeholder="Tags (comma-separated, optional)" className={`${FIELD} font-mono text-[11.5px]`} />
        <div className="flex flex-wrap items-center gap-2">
          <button
            type="submit"
            disabled={createMut.isPending || !slug.trim() || !summary.trim()}
            className="rounded-lg bg-primary px-3.5 py-1.5 text-[12.5px] font-medium text-primary-foreground transition-opacity hover:opacity-90 disabled:opacity-40"
          >
            {createMut.isPending ? "Saving…" : "Create & approve"}
          </button>
          <button type="button" onClick={onDone} className="px-2 py-1.5 text-[12px] text-muted-foreground hover:text-foreground">
            cancel
          </button>
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
      {p.t3codeThreadId && <span className="text-faint">thread {p.t3codeThreadId}</span>}
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
    <details onToggle={(e) => setOpen(e.currentTarget.open)} className="group">
      <summary className="cursor-pointer list-none font-mono text-[11px] text-faint marker:hidden hover:text-foreground">
        <span className="inline-block transition-transform group-open:rotate-90">›</span> history (rev {String(m.revision)}){m.edited && <span> · edited by {who(m.edited)}</span>}
      </summary>
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
    </details>
  );
}
