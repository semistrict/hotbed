import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate, useSearch } from "@tanstack/react-router";
import { useState, type DragEvent } from "react";
import { api, type Answer, type Question, type QuestionList, type QuestionOrder, type Snapshot, type Stamp } from "../api";
import { SessionLink } from "../components/SessionLink";
import { Card, Empty } from "../components/ui/Card";
import { useSnapshot } from "../lib/data";
import { ago } from "../lib/format";
import { answerReady, pick, placement, reorder, responseLine, shortHead, TAB_STATES, who, type QuestionTab } from "../lib/questions";
import { PageTitle } from "./Nodes";

export interface QuestionsSearch {
  tab?: QuestionTab;
  sort?: QuestionOrder;
}

const TABS: { tab: QuestionTab; label: string; empty: string }[] = [
  { tab: "inbox", label: "Inbox", empty: "No questions wait for you. An agent asks with hotbed question add." },
  { tab: "outbox", label: "Outbox", empty: "Every answer has been picked up by an agent." },
  { tab: "handled", label: "Handled", empty: "No agent has acknowledged an answer yet." },
  { tab: "deleted", label: "Deleted", empty: "No question has been deleted." },
  { tab: "all", label: "All", empty: "No questions yet." },
];

const SORTS: { sort: QuestionOrder; label: string }[] = [
  { sort: "rank", label: "your order" },
  { sort: "newest", label: "newest" },
  { sort: "oldest", label: "oldest" },
  { sort: "updated", label: "recently changed" },
];

export function QuestionsPage() {
  const { data: s } = useSnapshot();
  if (!s) return null;
  return <Questions s={s} />;
}

function Questions({ s }: { s: Snapshot }) {
  const search = useSearch({ from: "/questions" });
  const navigate = useNavigate({ from: "/questions" });
  const tab = search.tab ?? "inbox";
  // The inbox is in the user's order, which puts new questions on top: newest first until reordered.
  const sort = search.sort ?? (tab === "inbox" ? "rank" : "newest");
  const key = ["questions", tab, sort] as const;
  const list = useQuery({ queryKey: key, queryFn: () => api.questions({ states: TAB_STATES[tab], order: sort }), refetchInterval: 2000, placeholderData: keepPreviousData });
  const outbox = useQuery({ queryKey: ["questions", "outbox", "count"], queryFn: () => api.questions({ states: TAB_STATES.outbox }), refetchInterval: 2000 });
  const now = Date.parse(s.now);
  const movable = tab === "inbox" && sort === "rank";
  const questions = list.data?.questions ?? [];
  const move = useMove(key);
  const counts: Partial<Record<QuestionTab, number>> = { inbox: s.openQuestions, outbox: outbox.data?.total };

  return (
    <div className="space-y-6">
      <PageTitle
        title="Questions"
        sub="What agents asked you, every session's in one list. Answer when you like: an answer waits in the outbox until an agent reads it and marks it handled. Nothing is ever removed."
      />
      <div className="rise flex flex-wrap items-center gap-3">
        <div role="tablist" aria-label="Questions" className="flex rounded-lg border border-border bg-card/70 p-0.5">
          {TABS.map((t) => (
            <button
              key={t.tab}
              type="button"
              role="tab"
              aria-selected={tab === t.tab}
              onClick={() => navigate({ search: { tab: t.tab } })}
              className={`rounded-md px-3 py-1.5 text-[12.5px] transition-colors ${tab === t.tab ? "bg-muted text-foreground" : "text-muted-foreground hover:text-foreground"}`}
            >
              {t.label}
              {counts[t.tab] ? <span className={`ml-1.5 font-mono text-[10.5px] ${t.tab === "inbox" ? "text-primary" : "text-muted-foreground"}`}>{counts[t.tab]}</span> : null}
            </button>
          ))}
        </div>
        <label className="ml-auto flex items-center gap-2 font-mono text-[11px] text-muted-foreground">
          sort
          <select
            value={sort}
            onChange={(e) => navigate({ search: { tab, sort: e.target.value as QuestionOrder } })}
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
      {list.isError && <div className="rounded-xl border border-critical/40 bg-card px-4 py-2.5 font-mono text-[11.5px] text-critical">Could not list the questions: {String(list.error)}</div>}
      {move.error && <div className="rounded-xl border border-critical/40 bg-card px-4 py-2.5 font-mono text-[11.5px] text-critical">Could not move it: {String(move.error)}</div>}
      {list.data && questions.length === 0 ? (
        <Card>
          <Empty>{TABS.find((t) => t.tab === tab)?.empty}</Empty>
        </Card>
      ) : (
        <QuestionList s={s} questions={questions} now={now} movable={movable} onMove={(id, before) => move.mutate({ id, before })} />
      )}
      {tab === "inbox" && !movable && questions.length > 1 && <p className="font-mono text-[11px] text-faint">Sort by your order to rearrange the inbox.</p>}
    </div>
  );
}

/** Moves a question, showing it in its new place at once and putting it back if the controller refuses. */
function useMove(key: readonly unknown[]) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, before }: { id: number; before: number | null }) => api.moveQuestion(id, before),
    onMutate: async ({ id, before }) => {
      await qc.cancelQueries({ queryKey: key });
      const was = qc.getQueryData<QuestionList>(key);
      if (was) qc.setQueryData<QuestionList>(key, { ...was, questions: reorder(was.questions, id, before) });
      return { was };
    },
    onError: (_err, _vars, ctx) => {
      if (ctx?.was) qc.setQueryData(key, ctx.was);
    },
    onSettled: () => qc.invalidateQueries({ queryKey: ["questions"] }),
  });
}

/** The questions as cards; in the user's order, each can be dragged, or stepped up and down, to a new place. */
function QuestionList({ s, questions, now, movable, onMove }: { s: Snapshot; questions: Question[]; now: number; movable: boolean; onMove: (id: number, before: number | null) => void }) {
  const [dragged, setDragged] = useState<number | null>(null);
  const [drop, setDrop] = useState<{ id: number; after: boolean } | null>(null);
  const order = questions.map((q) => q.id);
  const moveTo = (id: number, to: number) => {
    const p = placement(order, id, to);
    if (p) onMove(id, p.before);
  };
  const over = (e: DragEvent, id: number) => {
    if (dragged === null) return;
    e.preventDefault();
    const box = e.currentTarget.getBoundingClientRect();
    setDrop({ id, after: e.clientY > box.top + box.height / 2 });
  };
  const dropped = (e: DragEvent) => {
    e.preventDefault();
    if (dragged !== null && drop) {
      const others = order.filter((x) => x !== dragged);
      moveTo(dragged, others.indexOf(drop.id) + (drop.after ? 1 : 0));
    }
    setDragged(null);
    setDrop(null);
  };

  return (
    <ol className="space-y-4">
      {questions.map((q, i) => (
        <li
          key={q.id}
          draggable={movable}
          onDragStart={(e) => {
            setDragged(q.id);
            e.dataTransfer.effectAllowed = "move";
          }}
          onDragEnd={() => {
            setDragged(null);
            setDrop(null);
          }}
          onDragOver={(e) => over(e, q.id)}
          onDrop={dropped}
          className={`relative ${dragged === q.id ? "opacity-40" : ""}`}
        >
          {drop?.id === q.id && dragged !== q.id && <span aria-hidden className={`absolute right-2 left-2 h-0.5 rounded-full bg-primary ${drop.after ? "-bottom-2.5" : "-top-2.5"}`} />}
          <QuestionCard
            s={s}
            q={q}
            now={now}
            delay={Math.min(i, 8) * 40}
            mover={
              movable ? (
                <Mover
                  first={i === 0}
                  last={i === questions.length - 1}
                  onUp={() => moveTo(q.id, i - 1)}
                  onDown={() => moveTo(q.id, i + 1)}
                />
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

const STATE_TAG: Record<Question["state"], { text: string; className: string }> = {
  open: { text: "● waiting for you", className: "text-primary" },
  answered: { text: "↗ in the outbox", className: "text-frost" },
  dismissed: { text: "↗ dismissed, in the outbox", className: "text-frost" },
  handled: { text: "✓ handled", className: "text-good-ink" },
  deleted: { text: "⊘ deleted", className: "text-faint" },
};

function QuestionCard({ s, q, now, delay, mover }: { s: Snapshot; q: Question; now: number; delay: number; mover: React.ReactNode }) {
  const tag = STATE_TAG[q.state];
  return (
    <Card delay={delay} className={q.state === "deleted" ? "opacity-70" : ""}>
      <div className="flex gap-3">
        {mover}
        <div className="min-w-0 flex-1 space-y-3">
          <header className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
            <span className="eyebrow text-foreground">{q.prompt.header}</span>
            <span className="font-mono text-[10.5px] text-faint">#{q.id}</span>
            <span className={`font-mono text-[10.5px] ${tag.className}`}>{tag.text}</span>
            {q.prompt.multiSelect && <span className="font-mono text-[10px] tracking-wider text-faint uppercase">pick any</span>}
            <span className="ml-auto text-[11px] text-muted-foreground">
              <StampLine s={s} st={q.asked} now={now} verb="asked" />
            </span>
          </header>
          <h2 className="display text-[19px] leading-snug font-medium text-balance">{q.prompt.question}</h2>
          <AskedFromLine q={q} />
          {q.state === "open" ? <AnswerForm q={q} /> : <Response s={s} q={q} now={now} />}
          <History s={s} q={q} now={now} />
        </div>
      </div>
    </Card>
  );
}

/** "asked by brave-otter on asahimbp, 12m ago"; the session links when the cockpit still lists it. */
function StampLine({ s, st, now, verb }: { s: Snapshot; st: Stamp; now: number; verb: string }) {
  const listed = st.session !== null && s.sessions.some((x) => x.id === st.session);
  return (
    <span title={new Date(st.at).toLocaleString()}>
      {verb} by {st.session === null ? <span className="text-foreground">you</span> : listed ? <SessionLink id={st.session} /> : <span className="font-mono text-[11px]">{st.session}</span>}
      {st.host && <span className="text-faint"> on {st.host}</span>}, {ago(st.at, now)}
    </span>
  );
}

function AskedFromLine({ q }: { q: Question }) {
  const f = q.askedFrom;
  if (!f) return null;
  const repo = (f.remote || f.dir).replace(/\.git$/, "").split(/[/:]/).pop();
  return (
    <details className="group text-[11.5px] text-muted-foreground">
      <summary className="cursor-pointer list-none font-mono text-[11px] marker:hidden hover:text-foreground">
        <span className="text-faint group-open:rotate-90 inline-block transition-transform">›</span> from {repo}
        {f.branch && <> @ {f.branch}</>}
        {f.head && <span className="text-faint"> {shortHead(f.head)}</span>}
        {f.dirtyFiles > 0 && <span className="text-warning"> · {f.dirtyFiles} changed</span>}
      </summary>
      <div className="mt-2 space-y-1 rounded-lg border border-border bg-muted/50 px-3 py-2 font-mono text-[10.5px]">
        <div>{f.dir}</div>
        {f.remote && <div className="text-faint">{f.remote}</div>}
        {f.head && <div className="text-faint">{f.head}</div>}
        {f.dirty.length > 0 && (
          <pre className="mt-1 whitespace-pre-wrap text-foreground">
            {f.dirty.join("\n")}
            {f.dirtyFiles > f.dirty.length && `\n… and ${f.dirtyFiles - f.dirty.length} more`}
          </pre>
        )}
      </div>
    </details>
  );
}

/** Answering an open question: its options, words of the user's own, a note; or dismissing it. */
function AnswerForm({ q }: { q: Question }) {
  const qc = useQueryClient();
  const [answer, setAnswer] = useState<Answer>({ picked: [], other: "", notes: "" });
  const [focused, setFocused] = useState<string | null>(null);
  const [dismissing, setDismissing] = useState(false);
  const [reason, setReason] = useState("");
  const settle = () => Promise.all([qc.invalidateQueries({ queryKey: ["questions"] }), qc.invalidateQueries({ queryKey: ["snapshot"] })]);
  const send = useMutation({ mutationFn: () => api.answerQuestion(q.id, answer), onSettled: settle });
  const dismiss = useMutation({ mutationFn: () => api.dismissQuestion(q.id, reason), onSettled: settle });
  const busy = send.isPending || dismiss.isPending;
  const withPreview = q.prompt.options.filter((o) => o.preview);
  const shown = q.prompt.options.find((o) => o.label === focused && o.preview) ?? q.prompt.options.find((o) => answer.picked.includes(o.label) && o.preview) ?? withPreview[0];
  const field = "w-full rounded-lg border border-border bg-background/60 px-3 py-2 text-[12.5px] placeholder:text-faint focus:border-primary/50 focus:ring-2 focus:ring-primary/20 focus:outline-none";

  return (
    <form
      className="space-y-3"
      onSubmit={(e) => {
        e.preventDefault();
        if (answerReady(answer)) send.mutate();
      }}
    >
      <div className={withPreview.length > 0 ? "grid gap-3 lg:grid-cols-[minmax(0,1fr)_minmax(0,1fr)]" : ""}>
        <div role={q.prompt.multiSelect ? "group" : "radiogroup"} aria-label={q.prompt.question} className="space-y-1.5">
          {q.prompt.options.map((o) => {
            const on = answer.picked.includes(o.label);
            return (
              <button
                key={o.label}
                type="button"
                role={q.prompt.multiSelect ? "checkbox" : "radio"}
                aria-checked={on}
                onClick={() => setAnswer({ ...answer, picked: pick(answer.picked, o.label, q.prompt.multiSelect) })}
                onFocus={() => setFocused(o.label)}
                onMouseEnter={() => setFocused(o.label)}
                className={`flex w-full items-start gap-2.5 rounded-lg border px-3 py-2 text-left transition-colors ${on ? "border-primary/60 bg-ember-wash" : "border-border hover:border-border-strong hover:bg-muted/60"}`}
              >
                <span aria-hidden className={`mt-px font-mono text-[12px] ${on ? "text-primary" : "text-faint"}`}>
                  {q.prompt.multiSelect ? (on ? "☑" : "☐") : on ? "◉" : "○"}
                </span>
                <span className="min-w-0">
                  <span className="block text-[13px] font-medium text-foreground">{o.label}</span>
                  <span className="block text-[12px] text-muted-foreground">{o.description}</span>
                </span>
              </button>
            );
          })}
        </div>
        {shown && (
          <div className="min-w-0 rounded-lg border border-border bg-muted/50">
            <div className="eyebrow border-b border-border px-3 py-1.5">{shown.label}</div>
            <pre className="overflow-x-auto px-3 py-2 font-mono text-[11px] leading-relaxed whitespace-pre text-foreground">{shown.preview}</pre>
          </div>
        )}
      </div>
      <textarea
        rows={2}
        value={answer.other}
        onChange={(e) => setAnswer({ ...answer, other: e.target.value })}
        placeholder={answer.picked.length > 0 ? "Anything to add?" : "Or answer in your own words"}
        className={field}
        aria-label="Your own answer"
      />
      <input value={answer.notes} onChange={(e) => setAnswer({ ...answer, notes: e.target.value })} placeholder="Note for the agent (optional)" className={field} aria-label="Note" />
      <div className="flex flex-wrap items-center gap-2">
        <button
          type="submit"
          disabled={!answerReady(answer) || busy}
          className="rounded-lg bg-primary px-3.5 py-1.5 text-[12.5px] font-medium text-primary-foreground transition-opacity hover:opacity-90 disabled:opacity-40"
        >
          {send.isPending ? "Sending…" : "Answer"}
        </button>
        {dismissing ? (
          <>
            <input value={reason} onChange={(e) => setReason(e.target.value)} placeholder="Why not (optional)" className={`${field} max-w-xs py-1.5`} aria-label="Why you dismiss it" autoFocus />
            <button type="button" disabled={busy} onClick={() => dismiss.mutate()} className="rounded-lg border border-border-strong px-3 py-1.5 text-[12.5px] text-foreground hover:bg-muted disabled:opacity-40">
              Dismiss
            </button>
            <button type="button" onClick={() => setDismissing(false)} className="px-2 py-1.5 text-[12px] text-muted-foreground hover:text-foreground">
              cancel
            </button>
          </>
        ) : (
          <button type="button" onClick={() => setDismissing(true)} className="rounded-lg px-3 py-1.5 text-[12.5px] text-muted-foreground hover:bg-muted hover:text-foreground">
            Dismiss…
          </button>
        )}
        {(send.error || dismiss.error) && <span className="font-mono text-[11px] text-critical">{String(send.error ?? dismiss.error)}</span>}
      </div>
    </form>
  );
}

/** What became of a question that is no longer open: the user's response, and who picked it up or deleted it. */
function Response({ s, q, now }: { s: Snapshot; q: Question; now: number }) {
  const responded = q.answered ?? q.dismissed;
  return (
    <div className="space-y-2 rounded-lg border border-border bg-muted/40 px-3 py-2.5 text-[12.5px]">
      {responded && (
        <div className="space-y-1.5">
          <div className="text-[11px] text-muted-foreground">
            <StampLine s={s} st={responded} now={now} verb={q.dismissed ? "dismissed" : "answered"} />
          </div>
          {q.answer && (
            <div className="flex flex-wrap items-center gap-1.5">
              {q.answer.picked.map((p) => (
                <span key={p} className="rounded-md bg-ember-wash px-2 py-0.5 font-medium text-primary">
                  {p}
                </span>
              ))}
              {q.answer.other && <span className="text-foreground">{q.answer.other}</span>}
            </div>
          )}
          {q.dismissed && <div className="text-foreground">{q.dismissed.reason || <span className="text-faint">no reason given</span>}</div>}
          {q.answer?.notes && <div className="text-muted-foreground">Note: {q.answer.notes}</div>}
        </div>
      )}
      {(q.state === "answered" || q.state === "dismissed") && <div className="font-mono text-[11px] text-frost">waiting for an agent to read it</div>}
      {q.handled && (
        <div className="text-[11px] text-good-ink">
          <StampLine s={s} st={q.handled} now={now} verb="handled" />
        </div>
      )}
      {q.deleted && (
        <div className="text-[11px] text-muted-foreground">
          <StampLine s={s} st={q.deleted} now={now} verb="deleted" />
          {q.deleted.reason && <span className="text-foreground">: {q.deleted.reason}</span>}
        </div>
      )}
    </div>
  );
}

/** Every change to the question, loaded when opened. */
function History({ s, q, now }: { s: Snapshot; q: Question; now: number }) {
  const [open, setOpen] = useState(false);
  const history = useQuery({ queryKey: ["question-history", q.id, q.updatedAt, q.rank], queryFn: () => api.questionHistory(q.id), enabled: open });
  return (
    <details onToggle={(e) => setOpen(e.currentTarget.open)} className="group">
      <summary className="cursor-pointer list-none font-mono text-[11px] text-faint marker:hidden hover:text-foreground">
        <span className="inline-block transition-transform group-open:rotate-90">›</span> history{q.edited && <span> · edited by {who(q.edited)}</span>}
      </summary>
      {history.isError && <div className="mt-2 font-mono text-[11px] text-critical">{String(history.error)}</div>}
      {history.data && (
        <ol className="mt-2 space-y-1.5 border-l border-border pl-3">
          {history.data.map((c) => (
            <li key={c.seq} className="text-[11.5px] text-muted-foreground">
              <span className="font-mono text-[11px] text-foreground">{c.what}</span> <StampLine s={s} st={c.by} now={now} verb="" />
              {c.by.reason && <span>: {c.by.reason}</span>}
              {c.what === "edited" && <div className="text-foreground">{c.question.prompt.question}</div>}
              {(c.what === "answered" || c.what === "dismissed") && <div className="text-foreground">{responseLine(c.question)}</div>}
            </li>
          ))}
        </ol>
      )}
    </details>
  );
}
