import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams, useSearch } from "@tanstack/react-router";
import { Paperclip, Plus, Send, SmilePlus, Tag, Trash2, Pencil, CheckSquare } from "lucide-react";
import { useState, type ChangeEvent } from "react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import {
  BoardTaskState,
  clients,
  projectScope,
  type BoardAttachment,
  type BoardMessage,
  type BoardTask,
  type BoardThreadSummary,
} from "../api";
import { Markdown } from "../components/Markdown";
import { Card, Empty } from "../components/Panel";
import { ProjectTag } from "../components/ProjectTag";
import { SessionLink } from "../components/SessionLink";
import { useSnapshot } from "../lib/data";
import { ago } from "../lib/format";
import { useProject } from "../lib/project";
import { PageTitle } from "../components/PageTitle";

export interface BoardSearch {
  q?: string;
  label?: string;
  by?: string;
}

const EMOJI_GLYPHS: Record<string, string> = {
  thumbs_up: "👍",
  thumbs_down: "👎",
  eyes: "👀",
  tada: "🎉",
  rocket: "🚀",
  heart: "❤️",
  fire: "🔥",
  check_mark: "✅",
  white_check_mark: "✅",
  thinking: "🤔",
  wave: "👋",
  sparkles: "✨",
};

const QUICK_REACTIONS = ["thumbs_up", "eyes", "tada", "rocket"] as const;

export function emojiDisplay(canonical: string): string {
  return EMOJI_GLYPHS[canonical] ?? `:${canonical}:`;
}

export function taskStateMeta(state: BoardTaskState): { label: string; className: string } {
  switch (state) {
    case BoardTaskState.READY:
      return { label: "ready", className: "border-primary/40 bg-primary/10 text-primary" };
    case BoardTaskState.CLAIMED:
      return { label: "claimed", className: "border-sky-500/40 bg-sky-500/10 text-sky-400" };
    case BoardTaskState.BLOCKED:
      return { label: "blocked", className: "border-amber-500/40 bg-amber-500/10 text-amber-400" };
    case BoardTaskState.WAITING:
      return { label: "waiting", className: "border-border bg-muted/40 text-muted-foreground" };
    case BoardTaskState.COMPLETED:
      return { label: "completed", className: "border-emerald-500/40 bg-emerald-500/10 text-emerald-400" };
    case BoardTaskState.FAILED:
      return { label: "failed", className: "border-destructive/40 bg-destructive/10 text-critical" };
    case BoardTaskState.CANCELED:
      return { label: "canceled", className: "border-border bg-muted/30 text-faint" };
    default:
      return { label: "unknown", className: "border-border text-muted-foreground" };
  }
}

export function parseLabelInput(raw: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const part of raw.split(/[\s,]+/)) {
    const trimmed = part.trim();
    if (!trimmed) continue;
    const eq = trimmed.indexOf("=");
    if (eq > 0) {
      out[trimmed.slice(0, eq).trim()] = trimmed.slice(eq + 1).trim();
    }
  }
  return out;
}

function parseLabelFilters(raw: string | undefined): { key: string; values?: string[] }[] {
  if (!raw?.trim()) return [];
  const out: { key: string; values?: string[] }[] = [];
  for (const part of raw.split(/\s+/)) {
    const trimmed = part.trim();
    if (!trimmed) continue;
    const eq = trimmed.indexOf("=");
    if (eq > 0) {
      out.push({
        key: trimmed.slice(0, eq),
        values: trimmed
          .slice(eq + 1)
          .split(",")
          .filter(Boolean),
      });
    } else {
      out.push({ key: trimmed });
    }
  }
  return out;
}

function AuthorTag({ msg, now }: { msg: BoardMessage; now: number }) {
  const handle = msg.author?.handle || msg.by?.actor || "unknown";
  const sessionId = msg.by?.sessionId || msg.author?.sessionId || "";
  return (
    <span className="inline-flex flex-wrap items-baseline gap-1.5 text-[11.5px] text-muted-foreground">
      <span className="font-mono font-medium text-foreground">{handle}</span>
      {sessionId && <SessionLink id={sessionId} />}
      {msg.by?.model && <span className="font-mono text-[10px] text-faint">{msg.by.model}</span>}
      <span className="text-faint">{ago(msg.createdAt, now)}</span>
      {msg.editedAt !== undefined && <span className="text-[10.5px] text-faint">(edited)</span>}
    </span>
  );
}

function LabelBadges({ labels }: { labels: Record<string, string> }) {
  const entries = Object.entries(labels).sort(([a], [b]) => a.localeCompare(b));
  if (entries.length === 0) return null;
  return (
    <span className="inline-flex flex-wrap items-center gap-1">
      {entries.map(([k, v]) => (
        <Badge key={k} variant="outline" className="h-5 px-1.5 font-mono text-[10px] text-muted-foreground">
          {k}={v}
        </Badge>
      ))}
    </span>
  );
}

function AttachmentList({ attachments }: { attachments: BoardAttachment[] }) {
  if (attachments.length === 0) return null;
  return (
    <div className="flex flex-wrap gap-2 pt-1">
      {attachments.map((att) => {
        const href = `/board/attachments/${encodeURIComponent(att.id)}`;
        const isImage = att.contentType.startsWith("image/");
        return (
          <a
            key={att.id}
            href={href}
            target="_blank"
            rel="noreferrer"
            className="group inline-flex flex-col gap-1 rounded-lg border border-border bg-background/50 p-2 text-[11.5px] hover:border-border-strong"
          >
            {isImage && <img src={href} alt={att.name} className="max-h-48 max-w-xs rounded object-contain" />}
            <span className="inline-flex items-center gap-1.5 font-mono text-[11px] text-muted-foreground group-hover:text-foreground">
              <Paperclip className="size-3" />
              {att.name}
              <span className="text-faint">({Number(att.size).toLocaleString()} B)</span>
            </span>
          </a>
        );
      })}
    </div>
  );
}

function StructuredBlocks({ msg }: { msg: BoardMessage }) {
  if (msg.blocks.length === 0) return null;
  return (
    <div className="space-y-1.5 pt-1">
      {msg.blocks.map((blk, i) => {
        let pretty = blk.dataJson;
        try {
          pretty = JSON.stringify(JSON.parse(blk.dataJson), null, 2);
        } catch {
          // keep raw
        }
        return (
          <div key={i} className="rounded-lg border border-border bg-background/60 p-2.5">
            <div className="mb-1 font-mono text-[10.5px] text-primary">
              {blk.schema}@{blk.version}
            </div>
            <pre className="overflow-x-auto font-mono text-[11px] text-foreground/85">{pretty}</pre>
          </div>
        );
      })}
    </div>
  );
}

export function BoardPage() {
  const [selectedProject] = useProject();
  const snap = useSnapshot();
  const search = useSearch({ from: "/board" });
  const navigate = useNavigate({ from: "/board" });
  const qc = useQueryClient();

  const [queryText, setQueryText] = useState(search.q ?? "");
  const [labelText, setLabelText] = useState(search.label ?? "");
  const [before, setBefore] = useState("");
  const [composeOpen, setComposeOpen] = useState(false);
  const [composeProject, setComposeProject] = useState("");
  const [composeText, setComposeText] = useState("");
  const [composeLabels, setComposeLabels] = useState("");
  const [composeAsTask, setComposeAsTask] = useState(false);
  const [composeAttachments, setComposeAttachments] = useState<BoardAttachment[]>([]);

  const projects = snap.data?.projects ?? [];
  const effectiveProject = selectedProject ?? (composeProject || projects[0]?.name || "");

  const threadsQ = useQuery({
    queryKey: ["board", "threads", selectedProject, search.q ?? "", search.label ?? "", search.by ?? "", before],
    queryFn: () =>
      clients.board.listBoardThreads({
        scope: projectScope(selectedProject),
        text: search.q ?? "",
        by: search.by ?? "",
        labels: parseLabelFilters(search.label),
        before,
        limit: 50,
      }),
    refetchInterval: 5000,
  });

  const uploadMut = useMutation({
    mutationFn: async (file: File) => {
      const bytes = new Uint8Array(await file.arrayBuffer());
      return clients.board.uploadBoardAttachment({ name: file.name, contentType: file.type || "application/octet-stream", data: bytes });
    },
    onSuccess: (att) => setComposeAttachments((prev) => [...prev, att]),
  });

  const postMut = useMutation({
    mutationFn: async () => {
      const labels = parseLabelInput(composeLabels);
      const attachmentIds = composeAttachments.map((a) => a.id);
      if (composeAsTask) {
        return clients.board.createBoardTask({
          project: effectiveProject,
          text: composeText,
          labels,
          attachmentIds,
        });
      }
      return clients.board.postBoardMessage({
        project: effectiveProject,
        text: composeText,
        labels,
        attachmentIds,
      });
    },
    onSuccess: () => {
      setComposeText("");
      setComposeLabels("");
      setComposeAttachments([]);
      setComposeAsTask(false);
      setComposeOpen(false);
      void qc.invalidateQueries({ queryKey: ["board"] });
      void qc.invalidateQueries({ queryKey: ["snapshot"] });
    },
  });

  const onFilePick = (e: ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (file) uploadMut.mutate(file);
    e.target.value = "";
  };

  const applyFilter = () => {
    setBefore("");
    void navigate({
      search: {
        q: queryText.trim() || undefined,
        label: labelText.trim() || undefined,
        by: search.by,
      },
    });
  };

  const now = Date.now();
  const threads = [...(threadsQ.data?.threads ?? [])].reverse();

  return (
    <div className="space-y-6">
      <PageTitle
        title="Board"
        sub={
          selectedProject
            ? `Threads, structured updates, and discussions in ${selectedProject}.`
            : "Project-scoped message board across all Hotbed projects."
        }
        right={
          <div className="flex items-center gap-2">
            <Link
              to="/tasks"
              className="inline-flex items-center gap-1.5 rounded-lg border border-border px-3 py-1.5 font-mono text-[11.5px] text-muted-foreground hover:border-border-strong hover:text-foreground"
            >
              <CheckSquare className="size-3.5" />
              Tasks
            </Link>
            <Button size="sm" onClick={() => setComposeOpen((v) => !v)}>
              <Plus className="mr-1 size-3.5" />
              New thread
            </Button>
          </div>
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
            <label className="inline-flex cursor-pointer items-center gap-1.5 font-mono text-[11.5px] text-muted-foreground">
              <input
                type="checkbox"
                checked={composeAsTask}
                onChange={(e) => setComposeAsTask(e.target.checked)}
                className="rounded border-border"
              />
              Create as task
            </label>
            <Input
              value={composeLabels}
              onChange={(e) => setComposeLabels(e.target.value)}
              placeholder="labels: status=open area=api"
              className="h-8 max-w-xs font-mono text-[12px]"
            />
            <label className="ml-auto inline-flex cursor-pointer items-center gap-1 rounded-md border border-border px-2.5 py-1 font-mono text-[11px] text-muted-foreground hover:border-border-strong hover:text-foreground">
              <Paperclip className="size-3" />
              Attach
              <input type="file" className="hidden" onChange={onFilePick} />
            </label>
          </div>
          <Textarea
            value={composeText}
            onChange={(e) => setComposeText(e.target.value)}
            placeholder="Write a thread update (Markdown, @mentions, or ```json#schema@1 blocks)..."
            rows={4}
            className="font-mono text-[12.5px]"
          />
          {composeAttachments.length > 0 && (
            <div className="flex flex-wrap gap-1.5">
              {composeAttachments.map((att) => (
                <Badge key={att.id} variant="secondary" className="font-mono text-[10.5px]">
                  {att.name}
                </Badge>
              ))}
            </div>
          )}
          {(postMut.isError || uploadMut.isError) && (
            <div className="font-mono text-[11.5px] text-critical">{String(postMut.error ?? uploadMut.error)}</div>
          )}
          <div className="flex justify-end gap-2">
            <Button variant="ghost" size="sm" onClick={() => setComposeOpen(false)}>
              Cancel
            </Button>
            <Button
              size="sm"
              disabled={postMut.isPending || !effectiveProject || (!composeText.trim() && composeAttachments.length === 0)}
              onClick={() => postMut.mutate()}
            >
              <Send className="mr-1.5 size-3.5" />
              {composeAsTask ? "Create task" : "Post thread"}
            </Button>
          </div>
        </Card>
      )}

      <form
        onSubmit={(e) => {
          e.preventDefault();
          applyFilter();
        }}
        className="flex flex-wrap items-center gap-2"
      >
        <Input
          value={queryText}
          onChange={(e) => setQueryText(e.target.value)}
          placeholder="Search board text…"
          className="h-8 max-w-xs text-[12.5px]"
        />
        <Input
          value={labelText}
          onChange={(e) => setLabelText(e.target.value)}
          placeholder="Filter labels (e.g. area=cli)"
          className="h-8 max-w-xs font-mono text-[12px]"
        />
        <Button type="submit" variant="outline" size="sm">
          Filter
        </Button>
        {(search.q || search.label || search.by || before) && (
          <Button
            type="button"
            variant="ghost"
            size="sm"
            onClick={() => {
              setQueryText("");
              setLabelText("");
              setBefore("");
              void navigate({ search: {} });
            }}
          >
            Reset
          </Button>
        )}
      </form>

      {threadsQ.isError && (
        <div className="font-mono text-[11.5px] text-critical">Could not load board: {String(threadsQ.error)}</div>
      )}

      {threadsQ.data && threads.length === 0 ? (
        <Card>
          <Empty>No board threads yet. Start one above or run `hotbed board post` from the CLI.</Empty>
        </Card>
      ) : (
        <ol className="space-y-3">
          {threads.map((summary) => (
            <ThreadRow key={String(summary.message?.id)} summary={summary} now={now} />
          ))}
        </ol>
      )}

      {threadsQ.data?.older && (
        <div className="flex justify-center">
          <Button variant="outline" size="sm" onClick={() => setBefore(threadsQ.data!.older)}>
            Load older threads
          </Button>
        </div>
      )}
    </div>
  );
}

function ThreadRow({ summary, now }: { summary: BoardThreadSummary; now: number }) {
  const m = summary.message;
  if (!m) return null;
  const taskMeta = m.task ? taskStateMeta(m.task.state) : null;
  const firstLine = m.deleted ? "(deleted)" : m.text.split("\n")[0] || "(attachment)";

  return (
    <li>
      <Card className="transition-colors hover:border-border-strong">
        <div className="space-y-2">
          <div className="flex flex-wrap items-center gap-2">
            <Link
              to="/board/$id"
              params={{ id: String(m.id) }}
              className="font-mono text-[11.5px] font-semibold text-primary hover:underline"
            >
              #{String(m.id)}
            </Link>
            <ProjectTag name={m.project} />
            {taskMeta && (
              <Badge variant="outline" className={`h-5 px-1.5 font-mono text-[10px] ${taskMeta.className}`}>
                task:{taskMeta.label}
              </Badge>
            )}
            <LabelBadges labels={m.labels} />
            <span className="ml-auto">
              <AuthorTag msg={m} now={now} />
            </span>
          </div>

          <Link to="/board/$id" params={{ id: String(m.id) }} className="block space-y-1">
            <div className="text-[14px] leading-snug font-medium text-foreground">{firstLine}</div>
            {m.text.includes("\n") && !m.deleted && (
              <div className="line-clamp-2 text-[12.5px] text-muted-foreground">
                {m.text.slice(m.text.indexOf("\n") + 1).trim()}
              </div>
            )}
          </Link>

          <div className="flex flex-wrap items-center gap-3 text-[11.5px] text-muted-foreground">
            <span className="font-mono">{summary.replies} {summary.replies === 1 ? "reply" : "replies"}</span>
            {m.attachments.length > 0 && (
              <span className="inline-flex items-center gap-1 font-mono">
                <Paperclip className="size-3" />
                {m.attachments.length}
              </span>
            )}
            {m.reactions.length > 0 && (
              <span className="inline-flex items-center gap-1.5">
                {m.reactions.map((r) => (
                  <span key={r.emoji} className="rounded border border-border px-1.5 py-0.5 font-mono text-[10.5px]">
                    {emojiDisplay(r.emoji)} {r.actors.length}
                  </span>
                ))}
              </span>
            )}
            {summary.last && summary.last.id !== m.id && (
              <span className="ml-auto inline-flex flex-wrap items-baseline gap-1.5 text-[11px] text-faint">
                last reply by {summary.last.author?.handle || summary.last.by?.actor}
                <SessionLink id={summary.last.by?.sessionId || summary.last.author?.sessionId || ""} empty={null} />
                {ago(summary.last.createdAt, now)}
              </span>
            )}
          </div>
        </div>
      </Card>
    </li>
  );
}

export function BoardThreadPage() {
  const { id } = useParams({ from: "/board/$id" });
  const msgId = BigInt(id);
  const qc = useQueryClient();

  const threadQ = useQuery({
    queryKey: ["board", "thread", id],
    queryFn: () => clients.board.getBoardThread({ id: msgId }),
    refetchInterval: 4000,
  });

  const [replyText, setReplyText] = useState("");
  const [replyAttachments, setReplyAttachments] = useState<BoardAttachment[]>([]);
  const [blockReason, setBlockReason] = useState("");
  const [cancelReason, setCancelReason] = useState("");

  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ["board"] });
    void qc.invalidateQueries({ queryKey: ["snapshot"] });
  };

  const replyMut = useMutation({
    mutationFn: () =>
      clients.board.postBoardMessage({
        re: threadQ.data?.message?.id ?? msgId,
        text: replyText,
        attachmentIds: replyAttachments.map((a) => a.id),
      }),
    onSuccess: () => {
      setReplyText("");
      setReplyAttachments([]);
      refresh();
    },
  });

  const uploadMut = useMutation({
    mutationFn: async (file: File) => {
      const bytes = new Uint8Array(await file.arrayBuffer());
      return clients.board.uploadBoardAttachment({ name: file.name, contentType: file.type || "application/octet-stream", data: bytes });
    },
    onSuccess: (att) => setReplyAttachments((prev) => [...prev, att]),
  });

  const adoptTaskMut = useMutation({
    mutationFn: () => clients.board.createBoardTask({ re: threadQ.data?.message?.id ?? msgId }),
    onSuccess: refresh,
  });
  const unblockMut = useMutation({
    mutationFn: () => clients.board.unblockBoardTask({ id: threadQ.data!.message!.id }),
    onSuccess: refresh,
  });
  const reopenMut = useMutation({
    mutationFn: () => clients.board.reopenBoardTask({ id: threadQ.data!.message!.id }),
    onSuccess: refresh,
  });
  const blockMut = useMutation({
    mutationFn: () => clients.board.blockBoardTask({ id: threadQ.data!.message!.id, reason: blockReason.trim() || "held in cockpit" }),
    onSuccess: () => {
      setBlockReason("");
      refresh();
    },
  });
  const cancelTaskMut = useMutation({
    mutationFn: () => clients.board.cancelBoardTask({ id: threadQ.data!.message!.id, reason: cancelReason.trim() || "canceled in cockpit" }),
    onSuccess: () => {
      setCancelReason("");
      refresh();
    },
  });
  const cancelSchedMut = useMutation({
    mutationFn: (schedId: bigint) => clients.board.cancelBoardSchedule({ id: schedId }),
    onSuccess: refresh,
  });

  const root = threadQ.data?.message;
  const replies = threadQ.data?.replies ?? [];
  const schedules = threadQ.data?.schedules ?? [];
  const now = Date.now();

  return (
    <div className="space-y-6">
      <PageTitle
        title={root ? `Thread #${String(root.id)}` : `Thread #${id}`}
        sub={root ? `Project ${root.project}` : "Board thread"}
        right={
          <div className="flex items-center gap-2">
            {root && !root.task && !root.deleted && (
              <Button variant="outline" size="sm" disabled={adoptTaskMut.isPending} onClick={() => adoptTaskMut.mutate()}>
                <CheckSquare className="mr-1.5 size-3.5" />
                Convert to task
              </Button>
            )}
            <Link
              to="/board"
              className="inline-block rounded-lg border border-border px-3 py-1.5 font-mono text-[11.5px] text-muted-foreground hover:border-border-strong hover:text-foreground"
            >
              ← board
            </Link>
          </div>
        }
      />

      {threadQ.isError && (
        <div className="font-mono text-[11.5px] text-critical">Could not load thread #{id}: {String(threadQ.error)}</div>
      )}

      {root?.task && (
        <TaskBanner
          task={root.task}
          now={now}
          blockReason={blockReason}
          setBlockReason={setBlockReason}
          cancelReason={cancelReason}
          setCancelReason={setCancelReason}
          onBlock={() => blockMut.mutate()}
          onUnblock={() => unblockMut.mutate()}
          onReopen={() => reopenMut.mutate()}
          onCancel={() => cancelTaskMut.mutate()}
          error={blockMut.error ?? unblockMut.error ?? reopenMut.error ?? cancelTaskMut.error}
        />
      )}

      {root && <MessageCard msg={root} now={now} onChanged={refresh} isRoot />}

      {replies.length > 0 && (
        <ol className="space-y-3 border-l-2 border-border pl-4">
          {replies.map((r) => (
            <li key={String(r.id)}>
              <MessageCard msg={r} now={now} onChanged={refresh} />
            </li>
          ))}
        </ol>
      )}

      {schedules.length > 0 && (
        <Card className="space-y-2">
          <div className="font-mono text-[11px] uppercase tracking-wider text-muted-foreground">
            Pending scheduled replies ({schedules.length})
          </div>
          {schedules.map((s) => (
            <div key={String(s.id)} className="flex items-center gap-2 text-[12px]">
              <span className="font-mono text-faint">sched#{String(s.id)}</span>
              <span className="truncate text-foreground">{s.text}</span>
              <span className="ml-auto font-mono text-[11px] text-muted-foreground">
                next {new Date(Number(s.nextAt)).toLocaleString()}
              </span>
              <Button variant="ghost" size="sm" onClick={() => cancelSchedMut.mutate(s.id)}>
                Cancel
              </Button>
            </div>
          ))}
        </Card>
      )}

      {root && (
        <Card className="space-y-3">
          <div className="flex items-center justify-between">
            <span className="font-mono text-[11px] uppercase tracking-wider text-muted-foreground">Reply</span>
            <label className="inline-flex cursor-pointer items-center gap-1 rounded-md border border-border px-2 py-0.5 font-mono text-[11px] text-muted-foreground hover:border-border-strong hover:text-foreground">
              <Paperclip className="size-3" />
              Attach
              <input
                type="file"
                className="hidden"
                onChange={(e) => {
                  const file = e.target.files?.[0];
                  if (file) uploadMut.mutate(file);
                  e.target.value = "";
                }}
              />
            </label>
          </div>
          <Textarea
            value={replyText}
            onChange={(e) => setReplyText(e.target.value)}
            placeholder="Write a reply…"
            rows={3}
            className="font-mono text-[12.5px]"
          />
          {replyAttachments.length > 0 && (
            <div className="flex flex-wrap gap-1.5">
              {replyAttachments.map((att) => (
                <Badge key={att.id} variant="secondary" className="font-mono text-[10.5px]">
                  {att.name}
                </Badge>
              ))}
            </div>
          )}
          {(replyMut.isError || uploadMut.isError) && (
            <div className="font-mono text-[11.5px] text-critical">{String(replyMut.error ?? uploadMut.error)}</div>
          )}
          <div className="flex justify-end">
            <Button
              size="sm"
              disabled={replyMut.isPending || (!replyText.trim() && replyAttachments.length === 0)}
              onClick={() => replyMut.mutate()}
            >
              <Send className="mr-1.5 size-3.5" />
              Post reply
            </Button>
          </div>
        </Card>
      )}
    </div>
  );
}

function TaskBanner({
  task,
  now,
  blockReason,
  setBlockReason,
  cancelReason,
  setCancelReason,
  onBlock,
  onUnblock,
  onReopen,
  onCancel,
  error,
}: {
  task: BoardTask;
  now: number;
  blockReason: string;
  setBlockReason: (v: string) => void;
  cancelReason: string;
  setCancelReason: (v: string) => void;
  onBlock: () => void;
  onUnblock: () => void;
  onReopen: () => void;
  onCancel: () => void;
  error: unknown;
}) {
  const meta = taskStateMeta(task.state);
  const finished =
    task.state === BoardTaskState.COMPLETED ||
    task.state === BoardTaskState.FAILED ||
    task.state === BoardTaskState.CANCELED;

  return (
    <Card className="space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        <Badge variant="outline" className={`font-mono text-[11px] ${meta.className}`}>
          {meta.label}
        </Badge>
        {task.owner && <span className="font-mono text-[11px] text-muted-foreground">owner={task.owner}</span>}
        {task.forActor && <span className="font-mono text-[11px] text-foreground">for={task.forActor}</span>}
        {task.claim && (
          <span className="inline-flex flex-wrap items-baseline gap-1.5 font-mono text-[11px] text-sky-400">
            claimed by {task.claim.actor}
            <SessionLink id={task.claim.sessionId} empty={null} />
            <span>(lease until {new Date(Number(task.claim.until)).toLocaleTimeString()})</span>
          </span>
        )}
        {task.needs.length > 0 && (
          <span className="inline-flex items-center gap-1 font-mono text-[11px] text-muted-foreground">
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
      </div>

      {task.blocked && (
        <div className="rounded-md border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-[12.5px] text-amber-300">
          <span className="font-mono font-semibold">blocked{task.blocked.kind ? ` (${task.blocked.kind})` : ""}:</span>{" "}
          {task.blocked.reason}
        </div>
      )}
      {task.result && (
        <div className="rounded-md border border-emerald-500/30 bg-emerald-500/10 px-3 py-2 text-[12.5px] text-emerald-300">
          <span className="font-mono font-semibold">result:</span> {task.result}
        </div>
      )}
      {task.reason && (
        <div className="rounded-md border border-border bg-muted/40 px-3 py-2 text-[12.5px] text-muted-foreground">
          <span className="font-mono font-semibold">reason:</span> {task.reason}
        </div>
      )}

      <div className="flex flex-wrap items-center gap-2 border-t border-border pt-2.5">
        {task.state === BoardTaskState.BLOCKED && (
          <Button size="sm" onClick={onUnblock}>
            Unblock task
          </Button>
        )}
        {task.state === BoardTaskState.FAILED && (
          <Button size="sm" onClick={onReopen}>
            Reopen task
          </Button>
        )}
        {!finished && task.state !== BoardTaskState.BLOCKED && (
          <div className="flex items-center gap-1.5">
            <Input
              value={blockReason}
              onChange={(e) => setBlockReason(e.target.value)}
              placeholder="Hold reason…"
              className="h-7 w-44 text-[12px]"
            />
            <Button size="sm" variant="outline" onClick={onBlock}>
              Block
            </Button>
          </div>
        )}
        {task.state !== BoardTaskState.COMPLETED && task.state !== BoardTaskState.CANCELED && (
          <div className="ml-auto flex items-center gap-1.5">
            <Input
              value={cancelReason}
              onChange={(e) => setCancelReason(e.target.value)}
              placeholder="Cancel reason…"
              className="h-7 w-40 text-[12px]"
            />
            <Button size="sm" variant="destructive" onClick={onCancel}>
              Cancel task
            </Button>
          </div>
        )}
      </div>
      {Boolean(error) && <div className="font-mono text-[11px] text-critical">{String(error)}</div>}
      <span className="sr-only">{now}</span>
    </Card>
  );
}

function MessageCard({
  msg,
  now,
  onChanged,
  isRoot = false,
}: {
  msg: BoardMessage;
  now: number;
  onChanged: () => void;
  isRoot?: boolean;
}) {
  const [editing, setEditing] = useState(false);
  const [editText, setEditText] = useState(msg.text);
  const [labelOpen, setLabelOpen] = useState(false);
  const [labelDraft, setLabelDraft] = useState("");

  const editMut = useMutation({
    mutationFn: () => clients.board.editBoardMessage({ id: msg.id, text: editText, ifRevision: msg.revision }),
    onSuccess: () => {
      setEditing(false);
      onChanged();
    },
  });
  const delMut = useMutation({
    mutationFn: () => clients.board.deleteBoardMessage({ id: msg.id }),
    onSuccess: onChanged,
  });
  const reactMut = useMutation({
    mutationFn: ({ emoji, remove }: { emoji: string; remove?: boolean }) => clients.board.reactBoardMessage({ id: msg.id, emoji, remove }),
    onSuccess: onChanged,
  });
  const labelMut = useMutation({
    mutationFn: () => {
      const set = parseLabelInput(labelDraft);
      const unset = labelDraft
        .split(/[\s,]+/)
        .filter((s) => s.startsWith("-") && !s.includes("="))
        .map((s) => s.slice(1));
      return clients.board.labelBoardMessage({ id: msg.id, set, unset });
    },
    onSuccess: () => {
      setLabelDraft("");
      setLabelOpen(false);
      onChanged();
    },
  });

  return (
    <Card className={msg.deleted ? "opacity-60" : ""}>
      <div className="space-y-2.5">
        <header className="flex flex-wrap items-center gap-2">
          <span className="font-mono text-[11px] text-faint">#{String(msg.id)}</span>
          {isRoot && <ProjectTag name={msg.project} />}
          <AuthorTag msg={msg} now={now} />
          <LabelBadges labels={msg.labels} />
          {!msg.deleted && (
            <div className="ml-auto flex items-center gap-1">
              <Button variant="ghost" size="sm" className="h-6 px-1.5" title="Edit labels" onClick={() => setLabelOpen((v) => !v)}>
                <Tag className="size-3" />
              </Button>
              <Button
                variant="ghost"
                size="sm"
                className="h-6 px-1.5"
                title="Edit message"
                onClick={() => {
                  setEditText(msg.text);
                  setEditing((v) => !v);
                }}
              >
                <Pencil className="size-3" />
              </Button>
              <Button variant="ghost" size="sm" className="h-6 px-1.5 text-muted-foreground hover:text-critical" title="Delete message" onClick={() => delMut.mutate()}>
                <Trash2 className="size-3" />
              </Button>
            </div>
          )}
        </header>

        {editing ? (
          <div className="space-y-2">
            <Textarea value={editText} onChange={(e) => setEditText(e.target.value)} rows={4} className="font-mono text-[12.5px]" />
            <div className="flex justify-end gap-2">
              <Button variant="ghost" size="sm" onClick={() => setEditing(false)}>
                Cancel
              </Button>
              <Button size="sm" disabled={editMut.isPending} onClick={() => editMut.mutate()}>
                Save edit
              </Button>
            </div>
          </div>
        ) : msg.deleted ? (
          <div className="font-mono text-[12px] italic text-faint">(message deleted)</div>
        ) : (
          <Markdown className="text-[13.5px] leading-relaxed text-foreground/90">{msg.text}</Markdown>
        )}

        <StructuredBlocks msg={msg} />
        <AttachmentList attachments={msg.attachments} />

        {labelOpen && (
          <form
            onSubmit={(e) => {
              e.preventDefault();
              labelMut.mutate();
            }}
            className="flex items-center gap-2 pt-1"
          >
            <Input
              value={labelDraft}
              onChange={(e) => setLabelDraft(e.target.value)}
              placeholder="key=value or -key"
              className="h-7 max-w-xs font-mono text-[11.5px]"
            />
            <Button type="submit" size="sm" variant="outline" disabled={labelMut.isPending}>
              Apply
            </Button>
          </form>
        )}

        {!msg.deleted && (
          <div className="flex flex-wrap items-center gap-1.5 pt-1">
            {msg.reactions.map((r) => (
              <button
                key={r.emoji}
                type="button"
                onClick={() => reactMut.mutate({ emoji: r.emoji })}
                title={r.actors.join(", ")}
                className="inline-flex items-center gap-1 rounded-full border border-border bg-background/60 px-2 py-0.5 font-mono text-[11px] hover:border-border-strong"
              >
                <span>{emojiDisplay(r.emoji)}</span>
                <span>{r.actors.length}</span>
              </button>
            ))}
            <span className="inline-flex items-center gap-1 pl-1">
              <SmilePlus className="size-3 text-faint" />
              {QUICK_REACTIONS.map((em) => (
                <button
                  key={em}
                  type="button"
                  onClick={() => reactMut.mutate({ emoji: em })}
                  className="rounded px-1 text-[12px] opacity-60 hover:bg-muted hover:opacity-100"
                  title={`React :${em}:`}
                >
                  {emojiDisplay(em)}
                </button>
              ))}
            </span>
          </div>
        )}

        {(editMut.isError || delMut.isError || reactMut.isError || labelMut.isError) && (
          <div className="font-mono text-[11px] text-critical">
            {String(editMut.error ?? delMut.error ?? reactMut.error ?? labelMut.error)}
          </div>
        )}
      </div>
    </Card>
  );
}
