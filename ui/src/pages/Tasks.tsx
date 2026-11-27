import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { Plus, Send } from "lucide-react";
import { useState } from "react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { BoardTaskState, clients, projectScope, type BoardThreadSummary } from "../api";
import { Card, Empty } from "../components/Panel";
import { ProjectTag } from "../components/ProjectTag";
import { SessionLink } from "../components/SessionLink";
import { useSnapshot } from "../lib/data";
import { ago } from "../lib/format";
import { useProject } from "../lib/project";
import { parseLabelInput, taskStateMeta } from "./Board";
import { PageTitle } from "../components/PageTitle";

export type TasksTab = "open" | "ready" | "claimed" | "blocked" | "waiting" | "completed" | "failed" | "canceled" | "all";

export interface TasksSearch {
  tab?: TasksTab;
}

const TABS: { tab: TasksTab; label: string; states?: BoardTaskState[]; includeAll?: boolean }[] = [
  { tab: "open", label: "Open" },
  { tab: "ready", label: "Ready", states: [BoardTaskState.READY] },
  { tab: "claimed", label: "Claimed", states: [BoardTaskState.CLAIMED] },
  { tab: "blocked", label: "Blocked", states: [BoardTaskState.BLOCKED] },
  { tab: "waiting", label: "Waiting", states: [BoardTaskState.WAITING] },
  { tab: "completed", label: "Completed", states: [BoardTaskState.COMPLETED] },
  { tab: "failed", label: "Failed", states: [BoardTaskState.FAILED] },
  { tab: "canceled", label: "Canceled", states: [BoardTaskState.CANCELED] },
  { tab: "all", label: "All", includeAll: true },
];

export function TasksPage() {
  const [selectedProject] = useProject();
  const snap = useSnapshot();
  const search = useSearch({ from: "/tasks" });
  const navigate = useNavigate({ from: "/tasks" });
  const qc = useQueryClient();

  const tab = search.tab ?? "open";
  const tabDef = TABS.find((t) => t.tab === tab) ?? TABS[0]!;

  const [composeOpen, setComposeOpen] = useState(false);
  const [composeProject, setComposeProject] = useState("");
  const [text, setText] = useState("");
  const [labels, setLabels] = useState("");
  const [forActor, setForActor] = useState("");
  const [needsText, setNeedsText] = useState("");

  const projects = snap.data?.projects ?? [];
  const effectiveProject = selectedProject ?? (composeProject || projects[0]?.name || "");

  const tasksQ = useQuery({
    queryKey: ["board", "tasks", selectedProject, tab],
    queryFn: () =>
      clients.board.listBoardTasks({
        scope: projectScope(selectedProject),
        states: tabDef.states,
        includeAll: tabDef.includeAll,
        latest: tab === "completed" || tab === "failed" || tab === "canceled" || tab === "all",
        limit: 100,
      }),
    refetchInterval: 4000,
  });

  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ["board"] });
    void qc.invalidateQueries({ queryKey: ["snapshot"] });
  };

  const createMut = useMutation({
    mutationFn: () => {
      const needs = needsText
        .split(/[\s,]+/)
        .map((s) => s.replace(/^#/, "").trim())
        .filter((s) => /^\d+$/.test(s))
        .map((s) => BigInt(s));
      return clients.board.createBoardTask({
        project: effectiveProject,
        text,
        labels: parseLabelInput(labels),
        forActor: forActor.trim(),
        needs,
      });
    },
    onSuccess: () => {
      setText("");
      setLabels("");
      setForActor("");
      setNeedsText("");
      setComposeOpen(false);
      refresh();
    },
  });

  const unblockMut = useMutation({ mutationFn: (id: bigint) => clients.board.unblockBoardTask({ id }), onSuccess: refresh });
  const reopenMut = useMutation({ mutationFn: (id: bigint) => clients.board.reopenBoardTask({ id }), onSuccess: refresh });

  const now = Date.now();
  const tasks = tasksQ.data?.tasks ?? [];

  return (
    <div className="space-y-6">
      <PageTitle
        title="Tasks"
        sub={
          selectedProject
            ? `Leased DAG task queue for ${selectedProject}.`
            : "Project-scoped DAG task queue across all Hotbed projects."
        }
        right={
          <Button size="sm" onClick={() => setComposeOpen((v) => !v)}>
            <Plus className="mr-1 size-3.5" />
            New task
          </Button>
        }
      />

      {composeOpen && (
        <Card className="space-y-3">
          <div className="flex flex-wrap items-center gap-2">
            {!selectedProject && (
              <select
                value={effectiveProject}
                onChange={(e) => setComposeProject(e.target.value)}
                aria-label="Target project"
                className="h-8 rounded-md border border-border bg-background px-2.5 font-mono text-[12px]"
              >
                {projects.map((p) => (
                  <option key={p.name} value={p.name}>
                    {p.name}
                  </option>
                ))}
              </select>
            )}
            <Input
              value={needsText}
              onChange={(e) => setNeedsText(e.target.value)}
              placeholder="needs: #101, #102"
              className="h-8 w-44 font-mono text-[12px]"
            />
            <Input
              value={forActor}
              onChange={(e) => setForActor(e.target.value)}
              placeholder="for: handle or actor"
              className="h-8 w-44 font-mono text-[12px]"
            />
            <Input
              value={labels}
              onChange={(e) => setLabels(e.target.value)}
              placeholder="labels: area=storage"
              className="h-8 w-48 font-mono text-[12px]"
            />
          </div>
          <Textarea
            value={text}
            onChange={(e) => setText(e.target.value)}
            placeholder="Describe the task specification…"
            rows={4}
            className="font-mono text-[12.5px]"
          />
          {createMut.isError && <div className="font-mono text-[11.5px] text-critical">{String(createMut.error)}</div>}
          <div className="flex justify-end gap-2">
            <Button variant="ghost" size="sm" onClick={() => setComposeOpen(false)}>
              Cancel
            </Button>
            <Button size="sm" disabled={createMut.isPending || !effectiveProject || !text.trim()} onClick={() => createMut.mutate()}>
              <Send className="mr-1.5 size-3.5" />
              Create task
            </Button>
          </div>
        </Card>
      )}

      <Tabs value={tab} onValueChange={(v) => navigate({ search: { tab: v as TasksTab } })}>
        <TabsList aria-label="Tasks" className="flex-wrap border border-border bg-card/70">
          {TABS.map((t) => (
            <TabsTrigger key={t.tab} value={t.tab} className="px-3 text-[12px]">
              {t.label}
            </TabsTrigger>
          ))}
        </TabsList>
      </Tabs>

      {tasksQ.isError && <div className="font-mono text-[11.5px] text-critical">Could not list tasks: {String(tasksQ.error)}</div>}

      {tasksQ.data && tasks.length === 0 ? (
        <Card>
          <Empty>No {tab === "all" ? "" : `${tab} `}tasks. Create one above or with `hotbed board task create`.</Empty>
        </Card>
      ) : (
        <ol className="space-y-3">
          {tasks.map((summary) => (
            <TaskRow
              key={String(summary.message?.id)}
              summary={summary}
              now={now}
              onUnblock={(id) => unblockMut.mutate(id)}
              onReopen={(id) => reopenMut.mutate(id)}
            />
          ))}
        </ol>
      )}
    </div>
  );
}

function TaskRow({
  summary,
  now,
  onUnblock,
  onReopen,
}: {
  summary: BoardThreadSummary;
  now: number;
  onUnblock: (id: bigint) => void;
  onReopen: (id: bigint) => void;
}) {
  const m = summary.message;
  if (!m || !m.task) return null;
  const task = m.task;
  const meta = taskStateMeta(task.state);
  const title = m.text.split("\n")[0] || `Task #${String(m.id)}`;

  return (
    <li>
      <Card className="transition-colors hover:border-border-strong">
        <div className="space-y-2">
          <div className="flex flex-wrap items-center gap-2">
            <Link to="/board/$id" params={{ id: String(m.id) }} className="font-mono text-[11.5px] font-semibold text-primary hover:underline">
              #{String(m.id)}
            </Link>
            <Badge variant="outline" className={`h-5 px-1.5 font-mono text-[10px] ${meta.className}`}>
              {meta.label}
            </Badge>
            <ProjectTag name={m.project} />
            {task.forActor && <span className="font-mono text-[11px] text-muted-foreground">for={task.forActor}</span>}
            {task.claim && (
              <span className="inline-flex flex-wrap items-baseline gap-1.5 font-mono text-[11px] text-sky-400">
                claimed by {task.claim.actor}
                <SessionLink id={task.claim.sessionId} empty={null} />
              </span>
            )}
            <span className="ml-auto font-mono text-[11px] text-faint">{ago(m.createdAt, now)}</span>
          </div>

          <Link to="/board/$id" params={{ id: String(m.id) }} className="block text-[14px] font-medium text-foreground hover:text-primary">
            {title}
          </Link>

          <div className="flex flex-wrap items-center gap-2 text-[11.5px] text-muted-foreground">
            {task.needs.length > 0 && (
              <span className="inline-flex items-center gap-1 font-mono text-[11px]">
                needs:
                {task.needs.map((nid) => (
                  <Link
                    key={String(nid)}
                    to="/board/$id"
                    params={{ id: String(nid) }}
                    className="rounded border border-border px-1.5 py-0.5 text-primary hover:border-border-strong"
                  >
                    #{String(nid)}
                  </Link>
                ))}
              </span>
            )}
            {task.blocked && (
              <span className="text-amber-400">
                blocked{task.blocked.kind ? ` (${task.blocked.kind})` : ""}: {task.blocked.reason}
              </span>
            )}
            {task.result && <span className="text-emerald-400">result: {task.result}</span>}
            {task.reason && <span className="text-muted-foreground">reason: {task.reason}</span>}
            <div className="ml-auto flex items-center gap-2">
              <span className="font-mono text-[11px] text-faint">
                {summary.replies} {summary.replies === 1 ? "reply" : "replies"}
              </span>
              {task.state === BoardTaskState.BLOCKED && (
                <Button size="sm" variant="outline" className="h-6 px-2 text-[11px]" onClick={() => onUnblock(m.id)}>
                  Unblock
                </Button>
              )}
              {task.state === BoardTaskState.FAILED && (
                <Button size="sm" variant="outline" className="h-6 px-2 text-[11px]" onClick={() => onReopen(m.id)}>
                  Reopen
                </Button>
              )}
            </div>
          </div>
        </div>
      </Card>
    </li>
  );
}
