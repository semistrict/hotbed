import { useIsMutating, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate, useSearch } from "@tanstack/react-router";
import { useMemo, useRef, useState, type DragEvent } from "react";
import {
  api,
  QUESTION_ORDER_FROM_SLUG,
  QuestionState,
  type ListQuestionsResponse,
  type PromptOption,
  type Question,
  type QuestionOrderSlug,
  type Snapshot,
  type Stamp,
} from "../api";
import { InlineMarkdown, Markdown } from "../components/Markdown";
import { SessionLink } from "../components/SessionLink";
import { Card, Empty } from "../components/ui/Card";
import { ProjectTag } from "../components/ProjectTag";
import { useProject, useScopedSnapshot } from "../lib/project";
import { remoteName } from "../lib/projects";
import { ago } from "../lib/format";
import { chooseOption, chooseOwnWords, draftAnswer, draftOf, draftReady, dropAt, EMPTY_DRAFT, mergeLatestQuestions, placement, reorder, responseLine, shortHead, TAB_STATES, who, type Draft, type QuestionTab } from "../lib/questions";
import { PageTitle } from "./Nodes";

export interface QuestionsSearch {
  tab?: QuestionTab;
  sort?: QuestionOrderSlug;
}

const TABS: { tab: QuestionTab; label: string; empty: string }[] = [
  { tab: "inbox", label: "Inbox", empty: "No questions wait for you. An agent asks with hotbed question add." },
  { tab: "outbox", label: "Outbox", empty: "Every answer has been picked up by an agent." },
  { tab: "handled", label: "Handled", empty: "No agent has acknowledged an answer yet." },
  { tab: "deleted", label: "Deleted", empty: "No question has been deleted." },
  { tab: "all", label: "All", empty: "No questions yet." },
];

const SORTS: { sort: QuestionOrderSlug; label: string }[] = [
  { sort: "rank", label: "your order" },
  { sort: "newest", label: "newest" },
  { sort: "oldest", label: "oldest" },
  { sort: "updated", label: "recently changed" },
];

export function QuestionsPage() {
  const { data: s } = useScopedSnapshot();
  if (!s) return null;
  return <Questions s={s} />;
}

function Questions({ s }: { s: Snapshot }) {
  const search = useSearch({ from: "/questions" });
  const navigate = useNavigate({ from: "/questions" });
  const tab = search.tab ?? "inbox";
  // The inbox is in the user's order, which puts new questions on top: newest first until reordered.
  const sort = search.sort ?? (tab === "inbox" ? "rank" : "newest");
  const [project] = useProject();
  const key = ["questions", project, tab, sort] as const;
  // No refresh while a move is on its way: one landing first would show the old order again.
  const moving = useIsMutating({ mutationKey: MOVE }) > 0;
  const list = useQuery({
    queryKey: key,
    queryFn: () => api.questions({ project, states: TAB_STATES[tab], order: QUESTION_ORDER_FROM_SLUG[sort] }),
    refetchInterval: moving ? false : 2000,
    // Only reuse previous data while staying within the same project scope;
    // switching projects must not render another project's actionable questions.
    placeholderData: (prev, prevQuery) => (prevQuery?.queryKey[1] === project ? prev : undefined),
  });
  const outbox = useQuery({ queryKey: ["questions", project, "outbox", "count"], queryFn: () => api.questions({ project, states: TAB_STATES.outbox }), refetchInterval: 2000 });
  const now = Number(s.now);
  const movable = tab === "inbox" && sort === "rank";
  const seenRef = useRef<{ project: string | null; map: Map<number, Question> }>({ project, map: new Map() });
  const rawQuestions = list.data?.questions;
  const questions = useMemo(() => {
    if (seenRef.current.project !== project) {
      seenRef.current = { project, map: new Map() };
    }
    return rawQuestions ? mergeLatestQuestions(seenRef.current.map, rawQuestions, TAB_STATES[tab], project) : [];
  }, [rawQuestions, tab, project]);
  const move = useMove(key);
  const counts: Partial<Record<QuestionTab, number>> = { inbox: s.openQuestions, outbox: outbox.data?.total };

  return (
    <div className="space-y-6 [overflow-anchor:auto]">
      <PageTitle
        title="Questions"
        sub={`What agents asked you about ${project ?? "every project"}: each project's sessions share one list. Answer when you like; an answer waits in the outbox until an agent reads it and marks it handled. Nothing is ever removed.`}
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
            onChange={(e) => navigate({ search: { tab, sort: e.target.value as QuestionOrderSlug } })}
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
      {list.data && !list.isPlaceholderData && questions.length === 0 ? (
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

const MOVE = ["move-question"];

/** Moves a question, showing it in its new place at once and putting it back if the controller refuses. */
function useMove(key: readonly unknown[]) {
  const qc = useQueryClient();
  return useMutation({
    mutationKey: MOVE,
    mutationFn: ({ id, before }: { id: number; before: number | null }) => api.moveQuestion(id, before),
    onMutate: async ({ id, before }) => {
      await qc.cancelQueries({ queryKey: key });
      const was = qc.getQueryData<ListQuestionsResponse>(key);
      if (was) qc.setQueryData<ListQuestionsResponse>(key, { ...was, questions: reorder(was.questions, id, before) });
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
    // Not a drop target for itself: letting go where it started moves nothing.
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

const STATE_TAG: Record<QuestionState, { text: string; className: string }> = {
  [QuestionState.UNSPECIFIED]: { text: "● waiting for you", className: "text-primary" },
  [QuestionState.OPEN]: { text: "● waiting for you", className: "text-primary" },
  [QuestionState.ANSWERED]: { text: "↗ in the outbox", className: "text-frost" },
  [QuestionState.DISMISSED]: { text: "↗ dismissed, in the outbox", className: "text-frost" },
  [QuestionState.HANDLED]: { text: "✓ handled", className: "text-good-ink" },
  [QuestionState.DELETED]: { text: "⊘ deleted", className: "text-faint" },
};

/** Refreshes what a change to a question touches: the lists, and the counts in the snapshot. */
function useSettle() {
  const qc = useQueryClient();
  return () => Promise.all([qc.invalidateQueries({ queryKey: ["questions"] }), qc.invalidateQueries({ queryKey: ["snapshot"] })]);
}

const LINK = "font-mono text-[11px] text-muted-foreground underline decoration-border-strong underline-offset-2 hover:text-foreground disabled:opacity-40";
const FIELD = "w-full rounded-lg border border-border bg-background/60 px-3 py-2 text-[12.5px] placeholder:text-faint focus:border-primary/50 focus:ring-2 focus:ring-primary/20 focus:outline-none";

function QuestionCard({ s, q, now, delay, mover }: { s: Snapshot; q: Question; now: number; delay: number; mover: React.ReactNode }) {
  const tag = STATE_TAG[q.state] ?? STATE_TAG[QuestionState.OPEN];
  const [project] = useProject();
  const [editing, setEditing] = useState(false);
  const showProject = project === null;
  const prompt = q.prompt;
  // The user still responds while no agent has acted on the response: open, or in the outbox.
  const answerable = q.state === QuestionState.OPEN || q.state === QuestionState.ANSWERED || q.state === QuestionState.DISMISSED;
  return (
    <Card delay={delay} className={q.state === QuestionState.DELETED ? "opacity-70" : ""}>
      <div className="flex gap-3">
        {mover}
        <div className="min-w-0 flex-1 space-y-3">
          <header className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
            <span className="eyebrow text-foreground">{prompt?.header ?? ""}</span>
            <span className="font-mono text-[10.5px] text-faint">#{q.id}</span>
            {showProject && <ProjectTag name={q.project} />}
            {q.repo && <span className="font-mono text-[10.5px] text-muted-foreground" title={q.repo}>{remoteName(q.repo)}</span>}
            <span className={`font-mono text-[10.5px] ${tag.className}`}>{tag.text}</span>
            {prompt?.multiSelect && <span className="font-mono text-[10px] tracking-wider text-faint uppercase">pick any</span>}
            {q.state === QuestionState.OPEN && (prompt?.options ?? []).some((o) => o.implemented) && <span className="font-mono text-[10.5px] text-good-ink">✓ went ahead</span>}
            {q.state !== QuestionState.DELETED && !editing && (
              <button type="button" onClick={() => setEditing(true)} className={LINK}>
                edit
              </button>
            )}
            {q.asked && (
              <span className="ml-auto text-[11px] text-muted-foreground">
                <StampLine s={s} st={q.asked} now={now} verb="asked" />
              </span>
            )}
          </header>
          {editing ? (
            <PromptEditor q={q} onDone={() => setEditing(false)} />
          ) : (
            <>
              <h2 className="text-[15px] leading-snug font-semibold text-pretty text-foreground">
                <InlineMarkdown>{prompt?.question ?? ""}</InlineMarkdown>
              </h2>
              {prompt?.body && <Markdown className="text-[13px] leading-relaxed text-foreground/85">{prompt.body}</Markdown>}
            </>
          )}
          <AskedFromLine q={q} />
          {answerable ? (
            <>
              {q.state !== QuestionState.OPEN && <OutboxLine s={s} q={q} now={now} />}
              {/* Starts from the response as it now is, and again whenever it changes. */}
              <AnswerForm key={String(q.updatedAt)} q={q} />
            </>
          ) : (
            <Response s={s} q={q} now={now} />
          )}
          <History s={s} q={q} now={now} />
        </div>
      </div>
    </Card>
  );
}

/** "asked by brave-otter on asahimbp, 12m ago"; the session links when the cockpit still lists it. */
function StampLine({ s, st, now, verb }: { s: Snapshot; st: Stamp; now: number; verb: string }) {
  const listed = st.session !== "" && s.sessions.some((x) => x.id === st.session);
  return (
    <span title={new Date(Number(st.at)).toLocaleString()}>
      {verb} by {st.session === "" ? <span className="text-foreground">you</span> : listed ? <SessionLink id={st.session} /> : <span className="font-mono text-[11px]">{st.session}</span>}
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

/** How an option the agent marked shows: the one it built, the one it would go on to. */
const MARKS = {
  implemented: { text: "✓ as implemented", tag: "bg-good/10 text-good-ink", row: "border-good/45 bg-good/5" },
  recommended: { text: "★ recommended", tag: "bg-frost-wash text-frost", row: "border-frost/45" },
} as const;

function markOf(o: PromptOption): keyof typeof MARKS | null {
  return o.implemented ? "implemented" : o.recommended ? "recommended" : null;
}

function MarkTag({ o }: { o: PromptOption }) {
  const mark = markOf(o);
  if (!mark) return null;
  return <span className={`shrink-0 rounded px-1.5 py-px font-mono text-[10px] whitespace-nowrap ${MARKS[mark].tag}`}>{MARKS[mark].text}</span>;
}

const OWN_WORDS = "In my own words";

/** Answering an open question: its options, words of the user's own, a note; or dismissing it. */
function AnswerForm({ q }: { q: Question }) {
  const settle = useSettle();
  const multi = q.prompt?.multiSelect ?? false;
  const options = q.prompt?.options ?? [];
  const [draft, setDraft] = useState<Draft>(() => (q.answer ? draftOf(q.answer) : EMPTY_DRAFT));
  const [focused, setFocused] = useState<string | null>(null);
  const [noting, setNoting] = useState(draft.notes !== "");
  const [dismissing, setDismissing] = useState(false);
  const [reason, setReason] = useState("");
  const send = useMutation({ mutationFn: (d: Draft) => api.answerQuestion(q.id, draftAnswer(d)), onSettled: settle });
  const dismiss = useMutation({ mutationFn: () => api.dismissQuestion(q.id, reason), onSettled: settle });
  const busy = send.isPending || dismiss.isPending;
  const choose = (label: string) => {
    if (busy) return;
    const next = chooseOption(draft, label, multi);
    setDraft(next.draft);
    if (next.submit) send.mutate(next.draft);
  };
  const withPreview = options.filter((o) => o.preview);
  const shown = options.find((o) => o.label === focused && o.preview) ?? options.find((o) => draft.picked.includes(o.label) && o.preview) ?? withPreview[0];
  // A single choice is sent by clicking it; only words of one's own, or several picks, need the button.
  const needsButton = multi || draft.ownWords;
  // Dismiss… and the Dismiss it becomes are one width, so the second click lands where the first did.
  const dismissWidth = "min-w-[5.75rem] text-left";

  return (
    <form
      className="space-y-3"
      onSubmit={(e) => {
        e.preventDefault();
        if (draftReady(draft) && !busy) send.mutate(draft);
      }}
    >
      <div className={withPreview.length > 0 ? "grid gap-3 lg:grid-cols-[minmax(0,1fr)_minmax(0,1fr)]" : ""}>
        <div role={multi ? "group" : "radiogroup"} aria-label={q.prompt?.question ?? ""} className="space-y-1.5">
          {options.map((o) => (
            <Choice key={o.label} multi={multi} on={draft.picked.includes(o.label)} busy={busy} mark={markOf(o)} onChoose={() => choose(o.label)} onFocus={() => setFocused(o.label)}>
              <span className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
                <span className="text-[13px] font-medium text-foreground">
                  <InlineMarkdown>{o.label}</InlineMarkdown>
                </span>
                <MarkTag o={o} />
              </span>
              <Markdown className="text-[12px] text-muted-foreground">{o.description}</Markdown>
            </Choice>
          ))}
          <Choice multi={multi} on={draft.ownWords} busy={busy} mark={null} onChoose={() => !busy && setDraft(chooseOwnWords(draft, multi))} onFocus={() => setFocused(null)}>
            <span className="block text-[13px] font-medium text-foreground">{OWN_WORDS}</span>
            <span className="block text-[12px] text-muted-foreground">{multi ? "Add an answer of your own." : "None of these: write the answer yourself."}</span>
          </Choice>
          {draft.ownWords && (
            <textarea
              rows={3}
              value={draft.other}
              onChange={(e) => setDraft({ ...draft, other: e.target.value })}
              placeholder="Your answer"
              className={FIELD}
              aria-label="Your own answer"
              autoFocus
            />
          )}
        </div>
        {shown && (
          <div className="min-w-0 rounded-lg border border-border bg-muted/50">
            <div className="eyebrow flex items-baseline gap-2 border-b border-border px-3 py-1.5">
              <span className="min-w-0 truncate">
                <InlineMarkdown>{shown.label}</InlineMarkdown>
              </span>
              <MarkTag o={shown} />
            </div>
            <pre className="overflow-x-auto px-3 py-2 font-mono text-[11px] leading-relaxed whitespace-pre text-foreground">{shown.preview}</pre>
          </div>
        )}
      </div>
      {noting ? (
        <input value={draft.notes} onChange={(e) => setDraft({ ...draft, notes: e.target.value })} placeholder="Note for the agent" className={FIELD} aria-label="Note" autoFocus />
      ) : (
        <button type="button" onClick={() => setNoting(true)} className={LINK}>
          + add a note
        </button>
      )}
      <div className="flex flex-wrap items-start gap-2">
        {needsButton && (
          <button
            type="submit"
            disabled={!draftReady(draft) || busy}
            className="rounded-lg bg-primary px-3.5 py-1.5 text-[12.5px] font-medium text-primary-foreground transition-opacity hover:opacity-90 disabled:opacity-40"
          >
            {send.isPending ? "Sending…" : "Answer"}
          </button>
        )}
        {q.state === QuestionState.DISMISSED ? null : dismissing ? (
          <>
            <button type="button" disabled={busy} onClick={() => dismiss.mutate()} className={`${dismissWidth} rounded-lg border border-border-strong px-3 py-1.5 text-[12.5px] text-foreground hover:bg-muted disabled:opacity-40`}>
              Dismiss
            </button>
            <input value={reason} onChange={(e) => setReason(e.target.value)} placeholder="Why not (optional)" className={`${FIELD} max-w-xs py-1.5`} aria-label="Why you dismiss it" />
            <button type="button" onClick={() => setDismissing(false)} className="px-2 py-1.5 text-[12px] text-muted-foreground hover:text-foreground">
              cancel
            </button>
          </>
        ) : (
          <button type="button" onClick={() => setDismissing(true)} className={`${dismissWidth} rounded-lg px-3 py-1.5 text-[12.5px] text-muted-foreground hover:bg-muted hover:text-foreground`}>
            Dismiss…
          </button>
        )}
        {(send.error || dismiss.error) && <span className="font-mono text-[11px] text-critical">{String(send.error ?? dismiss.error)}</span>}
      </div>
    </form>
  );
}

/**
 * One choice of an answer: a radio or a checkbox. Not a button, since its
 * markdown may hold links, which a button must not.
 */
function Choice({ multi, on, busy, mark, onChoose, onFocus, children }: { multi: boolean; on: boolean; busy: boolean; mark: keyof typeof MARKS | null; onChoose: () => void; onFocus: () => void; children: React.ReactNode }) {
  const look = on ? "border-primary/60 bg-ember-wash" : mark ? `${MARKS[mark].row} hover:border-border-strong` : "border-border hover:border-border-strong hover:bg-muted/60";
  return (
    <div
      role={multi ? "checkbox" : "radio"}
      aria-checked={on}
      aria-disabled={busy}
      tabIndex={0}
      onClick={onChoose}
      onKeyDown={(e) => {
        if (e.key === " " || e.key === "Enter") {
          e.preventDefault();
          onChoose();
        }
      }}
      onFocus={onFocus}
      onMouseEnter={onFocus}
      className={`flex w-full cursor-pointer items-start gap-2.5 rounded-lg border px-3 py-2 text-left transition-colors focus:ring-2 focus:ring-primary/30 focus:outline-none ${busy ? "pointer-events-none opacity-60" : ""} ${look}`}
    >
      <span aria-hidden className={`mt-px font-mono text-[12px] ${on ? "text-primary" : "text-faint"}`}>
        {multi ? (on ? "☑" : "☐") : on ? "◉" : "○"}
      </span>
      <span className="min-w-0 flex-1">{children}</span>
    </div>
  );
}

/** Rewriting a question's title and body; its options stay as they are. */
function PromptEditor({ q, onDone }: { q: Question; onDone: () => void }) {
  const settle = useSettle();
  const [question, setQuestion] = useState(q.prompt?.question ?? "");
  const [body, setBody] = useState(q.prompt?.body ?? "");
  const save = useMutation({
    mutationFn: () =>
      api.reviseQuestion(q.id, {
        header: q.prompt?.header ?? "",
        options: q.prompt?.options ?? [],
        multiSelect: q.prompt?.multiSelect ?? false,
        question,
        body,
      }),
    onSuccess: onDone,
    onSettled: settle,
  });
  return (
    <form
      className="space-y-2"
      onSubmit={(e) => {
        e.preventDefault();
        if (question.trim() !== "" && !save.isPending) save.mutate();
      }}
    >
      <input value={question} onChange={(e) => setQuestion(e.target.value)} className={`${FIELD} text-[14px] font-semibold`} aria-label="Question" autoFocus />
      <textarea value={body} onChange={(e) => setBody(e.target.value)} rows={Math.min(14, Math.max(3, body.split("\n").length + 1))} placeholder="Body: what the answer needs to know (markdown, optional)" className={`${FIELD} font-mono text-[12px]`} aria-label="Body" />
      <div className="flex flex-wrap items-center gap-2">
        <button type="submit" disabled={question.trim() === "" || save.isPending} className="rounded-lg bg-primary px-3.5 py-1.5 text-[12.5px] font-medium text-primary-foreground transition-opacity hover:opacity-90 disabled:opacity-40">
          {save.isPending ? "Saving…" : "Save"}
        </button>
        <button type="button" onClick={onDone} className="px-2 py-1.5 text-[12px] text-muted-foreground hover:text-foreground">
          cancel
        </button>
        {save.error && <span className="font-mono text-[11px] text-critical">{String(save.error)}</span>}
      </div>
    </form>
  );
}

/** Returning a question to the inbox, its response cleared. */
function ReopenButton({ q }: { q: Question }) {
  const settle = useSettle();
  const reopen = useMutation({ mutationFn: () => api.reopenQuestion(q.id), onSettled: settle });
  return (
    <>
      <button type="button" disabled={reopen.isPending} onClick={() => reopen.mutate()} className={LINK}>
        ↩ return to inbox
      </button>
      {reopen.error && <span className="font-mono text-[11px] text-critical">{String(reopen.error)}</span>}
    </>
  );
}

/** A response in the outbox: who gave it, that no agent has read it yet, and the way back to the inbox. */
function OutboxLine({ s, q, now }: { s: Snapshot; q: Question; now: number }) {
  const responded = q.answered ?? q.dismissed;
  return (
    <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1 text-[11px] text-muted-foreground">
      {responded && <StampLine s={s} st={responded} now={now} verb={q.dismissed ? "dismissed" : "answered"} />}
      {q.dismissed?.reason && <span className="text-foreground">{q.dismissed.reason}</span>}
      <span className="font-mono text-frost">waiting for an agent to read it; change it until then</span>
      <ReopenButton q={q} />
    </div>
  );
}

/** What became of a handled or deleted question: the user's response, and who acted on it or deleted it. */
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
              {q.answer.picked.map((p) => {
                const o = (q.prompt?.options ?? []).find((x) => x.label === p);
                return (
                  <span key={p} className="flex items-baseline gap-1.5 rounded-md bg-ember-wash px-2 py-0.5 font-medium text-primary">
                    <InlineMarkdown>{p}</InlineMarkdown>
                    {o && <MarkTag o={o} />}
                  </span>
                );
              })}
              {q.answer.other && <span className="text-foreground">{q.answer.other}</span>}
            </div>
          )}
          {q.dismissed && <div className="text-foreground">{q.dismissed.reason || <span className="text-faint">no reason given</span>}</div>}
          {q.answer?.notes && <div className="text-muted-foreground">Note: {q.answer.notes}</div>}
        </div>
      )}
      {q.handled && (
        <div className="flex flex-wrap items-baseline gap-x-3 text-[11px] text-good-ink">
          <StampLine s={s} st={q.handled} now={now} verb="handled" />
          <ReopenButton q={q} />
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
  const history = useQuery({ queryKey: ["question-history", q.id, String(q.updatedAt), String(q.rank)], queryFn: () => api.questionHistory(q.id), enabled: open });
  return (
    <details onToggle={(e) => setOpen(e.currentTarget.open)} className="group">
      <summary className="cursor-pointer list-none font-mono text-[11px] text-faint marker:hidden hover:text-foreground">
        <span className="inline-block transition-transform group-open:rotate-90">›</span> history{q.edited && <span> · edited by {who(q.edited)}</span>}
      </summary>
      {history.isError && <div className="mt-2 font-mono text-[11px] text-critical">{String(history.error)}</div>}
      {history.data && (
        <ol className="mt-2 space-y-1.5 border-l border-border pl-3">
          {history.data.map((c) => (
            <li key={String(c.seq)} className="text-[11.5px] text-muted-foreground">
              <span className="font-mono text-[11px] text-foreground">{c.what}</span> {c.by && <StampLine s={s} st={c.by} now={now} verb="" />}
              {c.by?.reason && <span>: {c.by.reason}</span>}
              {c.what === "edited" && c.question?.prompt && (
                <div className="text-foreground">
                  <InlineMarkdown>{c.question.prompt.question}</InlineMarkdown>
                </div>
              )}
              {(c.what === "answered" || c.what === "dismissed") && c.question && <div className="text-foreground">{responseLine(c.question)}</div>}
            </li>
          ))}
        </ol>
      )}
    </details>
  );
}
