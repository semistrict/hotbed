import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams, useSearch } from "@tanstack/react-router";
import { ChevronRightIcon, FileCode2, FileText } from "lucide-react";
import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import {
  clients,
  SKILL_ORDER_FROM_SLUG,
  SkillScope,
  SkillState,
  placeBefore,
  projectScope,
  type ListSkillsResponse,
  type Skill,
  type SkillOrderSlug,
  type Snapshot,
} from "../api";
import { Markdown } from "../components/Markdown";
import { PageTitle } from "../components/PageTitle";
import { Card, Empty } from "../components/Panel";
import { ProjectTag } from "../components/ProjectTag";
import { SortableList } from "../components/SortableList";
import { StampLine } from "../components/StampLine";
import { useProject, useScopedSnapshot } from "../lib/project";
import { remoteName } from "../lib/projects";
import { mergeLatestByRevision, useOptimisticReorder } from "../lib/rankedList";

export type SkillsTab = "active" | "archived" | "all";

export interface SkillsSearch {
  tab?: SkillsTab;
  scope?: "all" | "global" | "project";
  sort?: SkillOrderSlug;
  q?: string;
}

const TAB_STATES: Record<SkillsTab, SkillState[]> = {
  active: [SkillState.ACTIVE],
  archived: [SkillState.ARCHIVED],
  all: [SkillState.ACTIVE, SkillState.ARCHIVED],
};

const TABS: { tab: SkillsTab; label: string; empty: string }[] = [
  {
    tab: "active",
    label: "Active",
    empty: "No active skills yet. Install one with `hotbed skills add owner/repo` or create one here.",
  },
  { tab: "archived", label: "Archived", empty: "No archived skills." },
  { tab: "all", label: "All", empty: "No skills yet." },
];

const SORTS: { sort: SkillOrderSlug; label: string }[] = [
  { sort: "rank", label: "startup order" },
  { sort: "newest", label: "newest" },
  { sort: "oldest", label: "oldest" },
  { sort: "updated", label: "recently changed" },
  { sort: "name", label: "name" },
];

const GLOBAL = "__global__";
const ALL_REPOS = "__all_repos__";

function formatBytes(n: number | bigint): string {
  const b = Number(n);
  if (b >= 1 << 20) return `${(b / (1 << 20)).toFixed(1)} MiB`;
  if (b >= 1 << 10) return `${(b / (1 << 10)).toFixed(1)} KiB`;
  return `${b} B`;
}

function useSettleSkills() {
  const qc = useQueryClient();
  return () => Promise.all([qc.invalidateQueries({ queryKey: ["skills"] }), qc.invalidateQueries({ queryKey: ["skill"] })]);
}

export function SkillsPage() {
  const { data: s } = useScopedSnapshot();
  if (!s) return null;
  return <SkillsView s={s} />;
}

export function SkillDetailPage() {
  const { id } = useParams({ from: "/skills/$id" });
  const navigate = useNavigate();
  const { data: s } = useScopedSnapshot();
  const detail = useQuery({
    queryKey: ["skill", id],
    queryFn: () => clients.skills.showSkill({ ref: id }),
    refetchInterval: 3000,
  });
  if (!s) return null;
  return (
    <div className="space-y-6">
      <PageTitle
        title={detail.data ? detail.data.name : `Skill #${id}`}
        sub="Agent Skill manifest, files, upstream provenance, and revision history."
        right={
          <Link
            to="/skills"
            className="inline-block rounded-lg border border-border px-3 py-1.5 font-mono text-[11.5px] text-muted-foreground hover:border-border-strong hover:text-foreground"
          >
            ← all skills
          </Link>
        }
      />
      {detail.isError && (
        <div className="rounded-xl border border-critical/40 bg-card px-4 py-2.5 font-mono text-[11.5px] text-critical">
          {String(detail.error)}
        </div>
      )}
      {detail.isPending && !detail.data && (
        <div className="py-16 text-center font-mono text-[12px] text-muted-foreground">loading skill #{id}…</div>
      )}
      {detail.data && (
        <SkillCard
          sk={detail.data}
          now={Number(s.now)}
          delay={0}
          mover={null}
          defaultExpanded
          isFull
          onDeleted={() => void navigate({ to: "/skills" })}
        />
      )}
    </div>
  );
}

function SkillsView({ s }: { s: Snapshot }) {
  const search = useSearch({ from: "/skills" });
  const navigate = useNavigate({ from: "/skills" });
  const [project] = useProject();
  const [creating, setCreating] = useState(false);

  const tab: SkillsTab = search.tab ?? "active";
  const scopeSlug = search.scope ?? "all";
  const canRank = tab === "active";
  const availableSorts = useMemo(() => (canRank ? SORTS : SORTS.filter((o) => o.sort !== "rank")), [canRank]);
  const sortItems = useMemo(() => availableSorts.map((o) => ({ value: o.sort, label: o.label })), [availableSorts]);
  const sort: SkillOrderSlug =
    search.sort && (canRank || search.sort !== "rank") ? search.sort : canRank ? "rank" : "newest";
  const queryText = search.q ?? "";

  const [searchInput, setSearchInput] = useState(queryText);
  useEffect(() => {
    setSearchInput(queryText);
  }, [queryText]);

  useEffect(() => {
    if (searchInput === queryText) return;
    const t = setTimeout(() => {
      void navigate({ search: { ...search, tab, q: searchInput || undefined }, replace: true });
    }, 200);
    return () => clearTimeout(t);
  }, [searchInput, queryText, navigate, search, tab]);

  const scopeFilter =
    scopeSlug === "global" ? SkillScope.GLOBAL : scopeSlug === "project" ? SkillScope.PROJECT : SkillScope.UNSPECIFIED;

  const scopeFilterItems = useMemo(
    () => [
      { value: "all", label: project ? `global + ${project}` : "all scopes" },
      { value: "global", label: "global" },
      { value: "project", label: project ?? "projects" },
    ],
    [project],
  );

  const key = ["skills", project, tab, scopeSlug, sort, queryText] as const;
  const move = useOptimisticReorder<"skills", ListSkillsResponse>({
    key,
    field: "skills",
    mutationKey: ["move-skill"],
    move: (id, before) => clients.skills.moveSkill({ id, place: placeBefore(before) }),
    invalidate: ["skills"],
  });

  const list = useQuery({
    queryKey: key,
    queryFn: () =>
      clients.skills.listSkills({
        projectScope: scopeFilter === SkillScope.GLOBAL ? { case: undefined } : projectScope(project),
        scope: scopeFilter,
        states: TAB_STATES[tab],
        query: queryText,
        order: SKILL_ORDER_FROM_SLUG[sort],
      }),
    refetchInterval: move.moving ? false : 2500,
    placeholderData: (prev, prevQuery) => (prevQuery?.queryKey[1] === project ? prev : undefined),
  });

  const seenRef = useRef<{ project: string | null; map: Map<number, Skill> }>({ project, map: new Map() });
  const rawSkills = list.data?.skills;
  const skills = useMemo(() => {
    if (seenRef.current.project !== project) {
      seenRef.current = { project, map: new Map() };
    }
    return rawSkills ? mergeLatestByRevision(seenRef.current.map, rawSkills, TAB_STATES[tab]) : [];
  }, [rawSkills, tab, project]);

  const movable = canRank && sort === "rank" && !queryText && scopeSlug === "all";
  const hasFilters = Boolean(queryText || scopeSlug !== "all");
  const now = Number(s.now);
  const counts: Partial<Record<SkillsTab, number>> = {
    active: tab === "active" && !hasFilters ? list.data?.total : undefined,
  };

  return (
    <div className="space-y-6 [overflow-anchor:auto]">
      <PageTitle
        title="Skills"
        sub={`Agent Skills for ${project ? `global + ${project}` : "global + every project"}. Every active skill descriptor is announced on hotbed session start and served over MCP (SEP-2640).`}
        right={
          <Button type="button" onClick={() => setCreating((v) => !v)} className="text-[12.5px]">
            {creating ? "Cancel" : "+ New skill"}
          </Button>
        }
      />

      {creating && (
        <Card>
          <NewSkillForm s={s} defaultProject={project} onDone={() => setCreating(false)} />
        </Card>
      )}

      <div className="rise flex flex-wrap items-center gap-2.5">
        <div className="max-w-full overflow-x-auto no-scrollbar">
          <Tabs value={tab} onValueChange={(v) => navigate({ search: { ...search, tab: v as SkillsTab, sort: undefined } })}>
            <TabsList aria-label="Skills" className="border border-border bg-card/70">
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
          value={searchInput}
          onChange={(e) => setSearchInput(e.target.value)}
          placeholder="Search name, description, source…"
          aria-label="Search skills"
          className="w-full bg-card text-[12px] placeholder:text-faint sm:w-64"
        />

        <div className="flex flex-wrap items-center gap-x-3 gap-y-2 font-mono text-[11px] text-muted-foreground lg:ml-auto">
          <div className="flex items-center gap-1.5">
            <span>scope</span>
            <Select
              items={scopeFilterItems}
              value={scopeSlug}
              onValueChange={(v) =>
                v && navigate({ search: { ...search, tab, scope: v === "all" ? undefined : (v as "global" | "project") } })
              }
            >
              <SelectTrigger size="sm" aria-label="Filter by scope" className="bg-card font-mono text-[11.5px] text-foreground">
                <SelectValue />
              </SelectTrigger>
              <SelectContent className="font-mono text-[11.5px]">
                {scopeFilterItems.map((item) => (
                  <SelectItem key={item.value} value={item.value} className="text-[11.5px]">
                    {item.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          <div className="flex items-center gap-1.5">
            <span>sort</span>
            <Select
              items={sortItems}
              value={sort}
              onValueChange={(v) => v && navigate({ search: { ...search, tab, sort: v as SkillOrderSlug } })}
            >
              <SelectTrigger size="sm" aria-label="Sort skills" className="bg-card font-mono text-[11.5px] text-foreground">
                <SelectValue />
              </SelectTrigger>
              <SelectContent className="font-mono text-[11.5px]">
                {availableSorts.map((o) => (
                  <SelectItem key={o.sort} value={o.sort} className="text-[11.5px]">
                    {o.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
        </div>
      </div>

      {list.isError && (
        <Alert variant="destructive" className="rounded-xl border-critical/40 bg-card px-4 py-2.5 font-mono text-[11.5px]">
          <AlertDescription className="text-[11.5px] text-critical">Could not list skills: {String(list.error)}</AlertDescription>
        </Alert>
      )}
      {move.error && (
        <Alert variant="destructive" className="rounded-xl border-critical/40 bg-card px-4 py-2.5 font-mono text-[11.5px]">
          <AlertDescription className="text-[11.5px] text-critical">Could not reorder: {String(move.error)}</AlertDescription>
        </Alert>
      )}

      {list.data && !list.isPlaceholderData && skills.length === 0 ? (
        <Card>
          {hasFilters ? (
            <div className="flex flex-col items-center gap-3 py-8 text-center">
              <p className="font-mono text-[12px] text-muted-foreground">No skills match the current filters.</p>
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={() => {
                  setSearchInput("");
                  void navigate({ search: { ...search, tab, q: undefined, scope: undefined }, replace: true });
                }}
                className="font-mono text-[11.5px]"
              >
                Clear filters
              </Button>
            </div>
          ) : (
            <Empty>{TABS.find((t) => t.tab === tab)?.empty}</Empty>
          )}
        </Card>
      ) : (
        <SortableList
          items={skills}
          movable={movable}
          onMove={(id, before) => move.mutate({ id, before })}
          render={(sk, i, { mover }) => (
            <SkillCard sk={sk} now={now} delay={Math.min(i, 8) * 35} mover={mover} />
          )}
        />
      )}
      {tab === "active" && !movable && skills.length > 1 && (
        <p className="font-mono text-[11px] text-faint">Sort by startup order (with all scopes and no search filter) to drag and reorder.</p>
      )}
    </div>
  );
}

function SkillCard({
  sk,
  now,
  delay,
  mover,
  defaultExpanded = false,
  isFull = false,
  onDeleted,
}: {
  sk: Skill;
  now: number;
  delay: number;
  mover: ReactNode;
  defaultExpanded?: boolean;
  isFull?: boolean;
  onDeleted?: () => void;
}) {
  const [expanded, setExpanded] = useState(defaultExpanded);
  const [selectedFile, setSelectedFile] = useState<string>("SKILL.md");
  const [confirmRemove, setConfirmRemove] = useState(false);
  const settle = useSettleSkills();

  const detail = useQuery({
    queryKey: ["skill", String(sk.id), String(sk.revision)],
    queryFn: () => clients.skills.showSkill({ ref: String(sk.id) }),
    enabled: expanded && !isFull,
  });
  const full = detail.data ?? sk;

  const fileQuery = useQuery({
    queryKey: ["skill-file", String(sk.id), String(sk.revision), selectedFile],
    queryFn: () => clients.skills.getSkill({ ref: String(sk.id), path: selectedFile }),
    enabled: expanded,
  });

  const promote = useMutation({
    mutationFn: () => clients.skills.reviseSkill({ id: sk.id, global: true, ifRevision: full.revision }),
    onSettled: settle,
  });
  const archive = useMutation({
    mutationFn: () => clients.skills.archiveSkill({ id: sk.id, ifRevision: full.revision }),
    onSettled: settle,
  });
  const restore = useMutation({
    mutationFn: () => clients.skills.restoreSkill({ id: sk.id, ifRevision: full.revision }),
    onSettled: settle,
  });
  const remove = useMutation({
    mutationFn: () => clients.skills.deleteSkill({ id: sk.id, ifRevision: full.revision }),
    onSuccess: () => {
      setConfirmRemove(false);
      onDeleted?.();
    },
    onSettled: settle,
  });

  const busy = promote.isPending || archive.isPending || restore.isPending || remove.isPending;
  const actionError = promote.error ?? archive.error ?? restore.error ?? remove.error;
  const isArchived = sk.state === SkillState.ARCHIVED;
  const isProjectScoped = sk.scope === SkillScope.PROJECT && sk.project !== "";

  const activeFile = fileQuery.data?.files[0];
  const decodedContent = useMemo(() => {
    if (!activeFile) return null;
    if (activeFile.content.includes(0)) return null;
    try {
      return new TextDecoder("utf-8", { fatal: true }).decode(activeFile.content);
    } catch {
      return null;
    }
  }, [activeFile]);

  return (
    <Card delay={delay} className={isArchived ? "opacity-75" : ""}>
      <div className="flex gap-2 sm:gap-3">
        {mover}
        <div className="min-w-0 flex-1 space-y-3">
          <header className="flex flex-wrap items-center gap-x-2.5 gap-y-1">
            {isFull ? (
              <span className="break-all font-mono text-[13px] font-semibold text-foreground">{sk.name}</span>
            ) : (
              <Link
                to="/skills/$id"
                params={{ id: String(sk.id) }}
                className="break-all font-mono text-[13px] font-semibold text-foreground hover:underline"
              >
                {sk.name}
              </Link>
            )}
            <span className="font-mono text-[10.5px] text-faint">#{sk.id}</span>
            {sk.scope === SkillScope.GLOBAL || !sk.project ? (
              <span className="rounded-md border border-frost/40 bg-frost-wash px-1.5 py-px font-mono text-[10px] text-frost">
                global
              </span>
            ) : (
              <ProjectTag name={sk.project} />
            )}
            {sk.repo && (
              <span className="font-mono text-[10.5px] text-muted-foreground" title={sk.repo}>
                {remoteName(sk.repo)}
              </span>
            )}
            <span className="rounded-md border border-border px-1.5 py-px font-mono text-[10px] text-muted-foreground">
              {sk.fileCount} {sk.fileCount === 1 ? "file" : "files"} · {formatBytes(sk.totalBytes)}
            </span>
            <span className={`font-mono text-[10.5px] ${isArchived ? "text-faint" : "text-good-ink"}`}>
              {isArchived ? "⊘ archived" : "✓ active"}
            </span>
            {sk.created && (
              <span className="text-[11px] text-muted-foreground sm:ml-auto">
                <StampLine st={sk.created} now={now} verb="installed" />
              </span>
            )}
          </header>

          <div className="space-y-1.5">
            <p className="text-[13.5px] leading-snug text-foreground">{sk.description}</p>
            <div className="flex flex-wrap items-center gap-2 font-mono text-[10.5px] text-muted-foreground">
              <span className="truncate">{sk.uri}</span>
              {sk.source?.source && (
                <span>
                  · source: <span className="text-foreground">{sk.source.source}</span>
                  {sk.source.ref ? `@${sk.source.ref}` : ""}
                  {sk.source.commit ? ` (${sk.source.commit.slice(0, 8)})` : ""}
                </span>
              )}
            </div>
            {sk.tags.length > 0 && (
              <div className="flex flex-wrap gap-1.5 pt-0.5">
                {sk.tags.map((t) => (
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

          <Collapsible open={expanded} onOpenChange={setExpanded}>
            <CollapsibleTrigger className="group inline-flex cursor-pointer items-center gap-1 font-mono text-[11px] text-muted-foreground hover:text-foreground">
              <ChevronRightIcon className="size-3 text-faint transition-transform group-data-panel-open:rotate-90" />
              <span>{expanded ? "hide files & manifest" : "inspect files & SKILL.md"}</span>
            </CollapsibleTrigger>
            <CollapsibleContent>
              <div className="mt-2.5 space-y-3">
                {full.files.length > 0 && (
                  <div className="flex flex-wrap gap-1.5">
                    {full.files.map((f) => {
                      const active = f.path === selectedFile;
                      return (
                        <button
                          key={f.path}
                          type="button"
                          onClick={() => setSelectedFile(f.path)}
                          className={`inline-flex items-center gap-1.5 rounded-md border px-2 py-1 font-mono text-[11px] transition-colors ${
                            active
                              ? "border-primary/50 bg-ember-wash text-foreground"
                              : "border-border bg-card text-muted-foreground hover:border-border-strong hover:text-foreground"
                          }`}
                          title={`${f.path} · ${formatBytes(f.size)} · ${f.digest}`}
                        >
                          {f.path === "SKILL.md" ? <FileText className="size-3" /> : <FileCode2 className="size-3" />}
                          <span>{f.path}</span>
                          <span className="text-[10px] text-faint">{formatBytes(f.size)}</span>
                        </button>
                      );
                    })}
                  </div>
                )}

                <div className="rounded-lg border border-border bg-muted/30 px-3.5 py-2.5">
                  {fileQuery.isLoading ? (
                    <div className="font-mono text-[11px] text-faint">loading {selectedFile}…</div>
                  ) : fileQuery.isError ? (
                    <div className="font-mono text-[11px] text-critical">{String(fileQuery.error)}</div>
                  ) : decodedContent !== null ? (
                    selectedFile.endsWith(".md") ? (
                      <Markdown className="text-[13px] leading-relaxed text-foreground/90">{decodedContent}</Markdown>
                    ) : (
                      <pre className="overflow-x-auto font-mono text-[11.5px] leading-relaxed text-foreground/90">
                        {decodedContent}
                      </pre>
                    )
                  ) : activeFile ? (
                    <div className="font-mono text-[11px] text-muted-foreground">
                      Binary file ({activeFile.mimeType || "application/octet-stream"}, {formatBytes(activeFile.content.length)}).
                    </div>
                  ) : null}
                </div>
              </div>
            </CollapsibleContent>
          </Collapsible>

          <div className="flex flex-wrap items-center gap-1 pt-0.5">
            {!isArchived && isProjectScoped && (
              <Button
                type="button"
                size="xs"
                variant="ghost"
                disabled={busy}
                onClick={() => promote.mutate()}
                className="font-mono text-[11px] text-muted-foreground hover:text-foreground"
              >
                {promote.isPending ? "Promoting…" : "Promote to global"}
              </Button>
            )}
            {!isArchived ? (
              <Button
                type="button"
                size="xs"
                variant="ghost"
                disabled={busy}
                onClick={() => archive.mutate()}
                className="font-mono text-[11px] text-muted-foreground hover:text-foreground"
              >
                {archive.isPending ? "Archiving…" : "Archive"}
              </Button>
            ) : (
              <Button
                type="button"
                size="xs"
                variant="ghost"
                disabled={busy}
                onClick={() => restore.mutate()}
                className="font-mono text-[11px] text-muted-foreground hover:text-foreground"
              >
                {restore.isPending ? "Restoring…" : "↩ Restore"}
              </Button>
            )}

            {confirmRemove ? (
              <>
                <Button
                  type="button"
                  variant="destructive"
                  size="xs"
                  disabled={busy}
                  onClick={() => remove.mutate()}
                  className="border-critical/50 font-mono text-[11px] text-critical"
                >
                  {remove.isPending ? "Removing…" : "Confirm remove"}
                </Button>
                <Button
                  type="button"
                  size="xs"
                  variant="ghost"
                  disabled={busy}
                  onClick={() => setConfirmRemove(false)}
                  className="font-mono text-[11px] text-muted-foreground hover:text-foreground"
                >
                  Cancel
                </Button>
              </>
            ) : (
              <Button
                type="button"
                size="xs"
                variant="ghost"
                disabled={busy}
                onClick={() => setConfirmRemove(true)}
                className="font-mono text-[11px] text-muted-foreground hover:text-critical"
              >
                Remove
              </Button>
            )}

            {actionError && <span className="font-mono text-[11px] text-critical">{String(actionError)}</span>}
          </div>
        </div>
      </div>
    </Card>
  );
}

function NewSkillForm({ s, defaultProject, onDone }: { s: Snapshot; defaultProject: string | null; onDone: () => void }) {
  const settle = useSettleSkills();
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [scope, setScope] = useState<string>(defaultProject ?? GLOBAL);
  const [repo, setRepo] = useState<string>(ALL_REPOS);
  const [tags, setTags] = useState("");
  const [rationale, setRationale] = useState("");
  const [body, setBody] = useState("# Instructions\n\nDescribe step-by-step guidance for agents using this skill.\n");

  const isGlobal = scope === GLOBAL;
  const projectRepos = isGlobal ? [] : (s.projects.find((p) => p.name === scope)?.repos ?? []);
  const scopeItems = [
    { value: GLOBAL, label: "Global (all projects)" },
    ...s.projects.map((p) => ({ value: p.name, label: `Project: ${p.name}` })),
  ];
  const repoItems = [
    { value: ALL_REPOS, label: `All repos in ${scope}` },
    ...projectRepos.map((r) => ({ value: r, label: remoteName(r) })),
  ];

  const cleanName = name.trim().toLowerCase();
  const invalid = !cleanName || !description.trim();

  const save = useMutation({
    mutationFn: () => {
      const skillMd = `---\nname: ${cleanName}\ndescription: ${JSON.stringify(description.trim())}\n---\n\n${body}`;
      const bytes = new TextEncoder().encode(skillMd);
      return clients.skills.putSkill({
        name: cleanName,
        files: [{ path: "SKILL.md", content: bytes, mimeType: "text/markdown" }],
        global: isGlobal,
        project: isGlobal ? "" : scope,
        repo: isGlobal || repo === ALL_REPOS ? "" : repo,
        tags: tags
          .split(",")
          .map((t) => t.trim().toLowerCase())
          .filter(Boolean),
        rationale: rationale.trim(),
        frontmatterJson: JSON.stringify({ name: cleanName, description: description.trim() }),
      });
    },
    onSuccess: onDone,
    onSettled: settle,
  });

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        if (!invalid) save.mutate();
      }}
      className="space-y-3"
    >
      <div className="grid gap-3 sm:grid-cols-2">
        <label className="space-y-1">
          <span className="font-mono text-[11px] text-muted-foreground">Name (lowercase kebab-case)</span>
          <Input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="code-review"
            className="bg-background font-mono text-[12px]"
          />
        </label>
        <label className="space-y-1">
          <span className="font-mono text-[11px] text-muted-foreground">Scope</span>
          <Select
            items={scopeItems}
            value={scope}
            onValueChange={(v) => {
              if (!v) return;
              setScope(v);
              setRepo(ALL_REPOS);
            }}
          >
            <SelectTrigger className="w-full bg-background font-mono text-[12px]">
              <SelectValue />
            </SelectTrigger>
            <SelectContent className="font-mono text-[12px]">
              {scopeItems.map((item) => (
                <SelectItem key={item.value} value={item.value}>
                  {item.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </label>
      </div>

      {!isGlobal && projectRepos.length > 0 && (
        <label className="block space-y-1">
          <span className="font-mono text-[11px] text-muted-foreground">Repo (optional)</span>
          <Select items={repoItems} value={repo} onValueChange={(v) => v && setRepo(v)}>
            <SelectTrigger className="w-full bg-background font-mono text-[12px]">
              <SelectValue />
            </SelectTrigger>
            <SelectContent className="font-mono text-[12px]">
              {repoItems.map((item) => (
                <SelectItem key={item.value} value={item.value}>
                  {item.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </label>
      )}

      <label className="block space-y-1">
        <span className="font-mono text-[11px] text-muted-foreground">Description (shown on session start & MCP skills/list)</span>
        <Input
          value={description}
          onChange={(e) => setDescription(e.target.value)}
          placeholder="What this skill does and when an agent should load it."
          className="bg-background text-[12.5px]"
        />
      </label>

      <label className="block space-y-1">
        <span className="font-mono text-[11px] text-muted-foreground">SKILL.md body</span>
        <Textarea
          value={body}
          onChange={(e) => setBody(e.target.value)}
          rows={8}
          className="bg-background font-mono text-[12px]"
        />
      </label>

      <div className="grid gap-3 sm:grid-cols-2">
        <label className="space-y-1">
          <span className="font-mono text-[11px] text-muted-foreground">Tags (comma-separated, optional)</span>
          <Input
            value={tags}
            onChange={(e) => setTags(e.target.value)}
            placeholder="review, security"
            className="bg-background font-mono text-[12px]"
          />
        </label>
        <label className="space-y-1">
          <span className="font-mono text-[11px] text-muted-foreground">Rationale (optional)</span>
          <Input
            value={rationale}
            onChange={(e) => setRationale(e.target.value)}
            placeholder="Why this skill exists"
            className="bg-background text-[12px]"
          />
        </label>
      </div>

      {save.error && <div className="font-mono text-[11.5px] text-critical">{String(save.error)}</div>}

      <div className="flex items-center gap-2">
        <Button type="submit" size="sm" disabled={invalid || save.isPending} className="text-[12px]">
          {save.isPending ? "Saving…" : "Save skill"}
        </Button>
        <Button type="button" size="sm" variant="ghost" onClick={onDone} className="font-mono text-[11.5px]">
          Cancel
        </Button>
      </div>
    </form>
  );
}
