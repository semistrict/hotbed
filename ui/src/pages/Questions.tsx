import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams, useSearch } from "@tanstack/react-router";
import { ChevronRightIcon } from "lucide-react";
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
  placeBefore,
  projectScope,
  QUESTION_ORDER_FROM_SLUG,
  QuestionState,
  type ListQuestionsResponse,
  type PromptOption,
  type Question,
  type QuestionOrderSlug,
  type Snapshot,
} from "../api";
import { InlineMarkdown, Markdown } from "../components/Markdown";
import { Card, Empty } from "../components/Panel";
import { ProjectTag } from "../components/ProjectTag";
import { SortableList } from "../components/SortableList";
import { FromLine, StampLine } from "../components/StampLine";
import { useProject, useScopedSnapshot } from "../lib/project";
import { remoteName } from "../lib/projects";
import { chooseOption, chooseOwnWords, draftAnswer, draftOf, draftReady, EMPTY_DRAFT, responseLine, TAB_STATES, type Draft, type QuestionTab } from "../lib/questions";
import { mergeLatestByRevision, useOptimisticReorder } from "../lib/rankedList";
import { who } from "../lib/stamps";
import { PageTitle } from "../components/PageTitle";
import { T3ThreadTag } from "../components/T3ThreadTag";

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
  const canRank = tab === "inbox";
  const availableSorts = useMemo(() => (canRank ? SORTS : SORTS.filter((o) => o.sort !== "rank")), [canRank]);
  const sortItems = useMemo(() => availableSorts.map((o) => ({ value: o.sort, label: o.label })), [availableSorts]);
  // The inbox is in the user's order, which puts new questions on top: newest first until reordered.
  const sort: QuestionOrderSlug =
    search.sort && (canRank || search.sort !== "rank")
      ? search.sort
      : canRank
        ? "rank"
        : "newest";
  const [project] = useProject();
  const key = ["questions", project, tab, sort] as const;
  const move = useOptimisticReorder<"questions", ListQuestionsResponse>({
    key,
    field: "questions",
    mutationKey: ["move-question"],
    move: (id, before) => clients.questions.moveQuestion({ id, place: placeBefore(before) }),
    invalidate: ["questions"],
  });
  const list = useQuery({
    queryKey: key,
    queryFn: () => clients.questions.listQuestions({ scope: projectScope(project), states: TAB_STATES[tab], order: QUESTION_ORDER_FROM_SLUG[sort] }),
    refetchInterval: move.moving ? false : 2000,
    // Only reuse previous data while staying within the same project scope;
    // switching projects must not render another project's actionable questions.
    placeholderData: (prev, prevQuery) => (prevQuery?.queryKey[1] === project ? prev : undefined),
  });
  const outbox = useQuery({ queryKey: ["questions", project, "outbox", "count"], queryFn: () => clients.questions.listQuestions({ scope: projectScope(project), states: TAB_STATES.outbox, order: QUESTION_ORDER_FROM_SLUG.newest }), refetchInterval: 2000 });
  const now = Number(s.now);
  const movable = canRank && sort === "rank";
  const seenRef = useRef<{ project: string | null; map: Map<number, Question> }>({ project, map: new Map() });
  const rawQuestions = list.data?.questions;
  const questions = useMemo(() => {
    if (seenRef.current.project !== project) {
      seenRef.current = { project, map: new Map() };
    }
    const inProject = project ? (q: Question) => q.project === project : undefined;
    return rawQuestions ? mergeLatestByRevision(seenRef.current.map, rawQuestions, TAB_STATES[tab], inProject) : [];
  }, [rawQuestions, tab, project]);
  const counts: Partial<Record<QuestionTab, number>> = { inbox: s.openQuestions, outbox: outbox.data?.total };

  return (
    <div className="space-y-6 [overflow-anchor:auto]">
      <PageTitle
        title="Questions"
        sub={`What agents asked you about ${project ?? "every project"}: each project's sessions share one list. Answer when you like; an answer waits in the outbox until an agent reads it and marks it handled. Nothing is ever removed.`}
      />
      <div className="rise flex flex-wrap items-center gap-2.5 sm:gap-3">
        <div className="max-w-full overflow-x-auto no-scrollbar">
          <Tabs value={tab} onValueChange={(v) => navigate({ search: { tab: v as QuestionTab } })}>
            <TabsList aria-label="Questions" className="border border-border bg-card/70">
              {TABS.map((t) => (
                <TabsTrigger key={t.tab} value={t.tab} className="px-2.5 text-[12px] sm:px-3 sm:text-[12.5px]">
                  {t.label}
                  {counts[t.tab] ? <span className={`ml-1 font-mono text-[10.5px] ${t.tab === "inbox" ? "text-primary" : "text-muted-foreground"}`}>{counts[t.tab]}</span> : null}
                </TabsTrigger>
              ))}
            </TabsList>
          </Tabs>
        </div>
        <div className="flex items-center gap-2 font-mono text-[11px] text-muted-foreground sm:ml-auto">
          <span>sort</span>
          <Select items={sortItems} value={sort} onValueChange={(v) => v && navigate({ search: { tab, sort: v as QuestionOrderSlug } })}>
            <SelectTrigger size="sm" aria-label="Sort questions" className="bg-card font-mono text-[11.5px] text-foreground">
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
      {list.isError && (
        <Alert variant="destructive" className="rounded-xl border-critical/40 bg-card px-4 py-2.5 font-mono text-[11.5px]">
          <AlertDescription className="text-[11.5px] text-critical">Could not list the questions: {String(list.error)}</AlertDescription>
        </Alert>
      )}
      {move.error && (
        <Alert variant="destructive" className="rounded-xl border-critical/40 bg-card px-4 py-2.5 font-mono text-[11.5px]">
          <AlertDescription className="text-[11.5px] text-critical">Could not move it: {String(move.error)}</AlertDescription>
        </Alert>
      )}
      {list.data && !list.isPlaceholderData && questions.length === 0 ? (
        <Card>
          <Empty>{TABS.find((t) => t.tab === tab)?.empty}</Empty>
        </Card>
      ) : (
        <SortableList
          items={questions}
          movable={movable}
          onMove={(id, before) => move.mutate({ id, before })}
          render={(q, i, { mover, onEditingChange }) => (
            <QuestionCard q={q} now={now} delay={Math.min(i, 8) * 40} mover={mover} onEditingChange={onEditingChange} />
          )}
        />
      )}
      {tab === "inbox" && !movable && questions.length > 1 && <p className="font-mono text-[11px] text-faint">Sort by your order to rearrange the inbox.</p>}
    </div>
  );
}

/** One question on a page of its own: where a link an agent sends leads. */
export function QuestionDetailPage() {
  const { id } = useParams({ from: "/questions/$id" });
  const { data: s } = useScopedSnapshot();
  // Under ["questions"], so what an answer refreshes includes it.
  const q = useQuery({ queryKey: ["questions", "one", id], queryFn: () => clients.questions.getQuestion({ id: Number(id) }), refetchInterval: 2000 });
  if (!s) return null;
  return (
    <div className="space-y-6">
      <PageTitle
        title={`Question #${id}`}
        sub="What an agent asked you. Answer it here; the answer waits in the outbox until the agent reads it."
        right={
          <Link to="/questions" className="inline-block rounded-lg border border-border px-3 py-1.5 font-mono text-[11.5px] text-muted-foreground hover:border-border-strong hover:text-foreground">
            ← all questions
          </Link>
        }
      />
      {q.isError && (
        <Alert variant="destructive" className="rounded-xl border-critical/40 bg-card px-4 py-2.5 font-mono text-[11.5px]">
          <AlertDescription className="text-[11.5px] text-critical">Could not load question #{id}: {String(q.error)}</AlertDescription>
        </Alert>
      )}
      {q.isPending && <div className="py-16 text-center font-mono text-[12px] text-muted-foreground">loading question #{id}…</div>}
      {q.data && <QuestionCard q={q.data} now={Number(s.now)} delay={0} mover={null} />}
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

function QuestionCard({
  q,
  now,
  delay,
  mover,
  onEditingChange,
}: {
  q: Question;
  now: number;
  delay: number;
  mover: ReactNode;
  onEditingChange?: (id: number, editing: boolean) => void;
}) {
  const tag = STATE_TAG[q.state] ?? STATE_TAG[QuestionState.OPEN];
  const [project] = useProject();
  const [editing, setEditing] = useState(false);
  useEffect(() => {
    onEditingChange?.(q.id, editing);
    return () => onEditingChange?.(q.id, false);
  }, [q.id, editing, onEditingChange]);
  const showProject = project === null;
  const prompt = q.prompt;
  // The user still responds while no agent has acted on the response: open, or in the outbox.
  const answerable = q.state === QuestionState.OPEN || q.state === QuestionState.ANSWERED || q.state === QuestionState.DISMISSED;
  return (
    <Card delay={delay} className={q.state === QuestionState.DELETED ? "opacity-70" : ""}>
      <div className="flex gap-2 sm:gap-3">
        {mover}
        <div className="min-w-0 flex-1 space-y-3">
          <header className="flex flex-wrap items-baseline gap-x-2.5 gap-y-1 sm:gap-x-3">
            <span className="eyebrow text-foreground">{prompt?.header ?? ""}</span>
            <Link to="/questions/$id" params={{ id: String(q.id) }} className="font-mono text-[10.5px] text-faint hover:text-foreground" title="This question's own page">
              #{q.id}
            </Link>
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
              <span className="text-[11px] text-muted-foreground sm:ml-auto">
                <StampLine st={q.asked} now={now} verb="asked" />
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
              {q.state !== QuestionState.OPEN && <OutboxLine q={q} now={now} />}
              <AnswerForm q={q} />
            </>
          ) : (
            <Response q={q} now={now} />
          )}
          <History q={q} now={now} />
        </div>
      </div>
    </Card>
  );
}

function AskedFromLine({ q }: { q: Question }) {
  const f = q.askedFrom;
  if (!f && !q.t3codeThreadId) return null;
  if (!f) {
    return (
      <div className="font-mono text-[11px] text-muted-foreground">
        <T3ThreadTag id={q.t3codeThreadId} />
      </div>
    );
  }
  return (
    <Collapsible className="text-[11.5px] text-muted-foreground">
      <CollapsibleTrigger className="group inline-flex cursor-pointer items-center gap-1 font-mono text-[11px] hover:text-foreground">
        <ChevronRightIcon className="size-3 text-faint transition-transform group-data-panel-open:rotate-90" />
        <span>
          <FromLine f={f} />
          {q.t3codeThreadId && (
            <span className="text-faint">
              {" "}· <T3ThreadTag id={q.t3codeThreadId} />
            </span>
          )}
        </span>
      </CollapsibleTrigger>
      <CollapsibleContent>
        <div className="mt-2 space-y-1 break-all rounded-lg border border-border bg-muted/50 px-3 py-2 font-mono text-[10.5px]">
          <div>{f.dir}</div>
          {f.remote && <div className="text-faint">{f.remote}</div>}
          {f.head && <div className="text-faint">{f.head}</div>}
          {q.t3codeThreadId && <div className="text-faint">thread {q.t3codeThreadId}</div>}
          {f.dirty.length > 0 && (
            <pre className="mt-1 whitespace-pre-wrap text-foreground">
              {f.dirty.join("\n")}
              {f.dirtyFiles > f.dirty.length && `\n… and ${f.dirtyFiles - f.dirty.length} more`}
            </pre>
          )}
        </div>
      </CollapsibleContent>
    </Collapsible>
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
  return (
    <Badge variant="secondary" className={`h-auto shrink-0 rounded px-1.5 py-px font-mono text-[10px] font-normal ${MARKS[mark].tag}`}>
      {MARKS[mark].text}
    </Badge>
  );
}

const OWN_WORDS = "In my own words";

function draftsEqual(a: Draft, b: Draft): boolean {
  return (
    a.ownWords === b.ownWords &&
    a.other === b.other &&
    a.notes === b.notes &&
    a.picked.length === b.picked.length &&
    a.picked.every((p, i) => p === b.picked[i])
  );
}

/** Answering an open question: its options, words of the user's own, a note; or dismissing it. */
function AnswerForm({ q }: { q: Question }) {
  const settle = useSettle();
  const multi = q.prompt?.multiSelect ?? false;
  const options = q.prompt?.options ?? [];
  const serverDraft = useMemo(() => (q.answer ? draftOf(q.answer) : EMPTY_DRAFT), [q.answer]);
  const [baseline, setBaseline] = useState<Draft>(serverDraft);
  const [draft, setDraft] = useState<Draft>(serverDraft);
  const [focused, setFocused] = useState<string | null>(null);
  const [noting, setNoting] = useState(serverDraft.notes !== "");
  const [dismissing, setDismissing] = useState(false);
  const [reason, setReason] = useState("");
  const dirty = !draftsEqual(draft, baseline);

  useEffect(() => {
    if (draftsEqual(serverDraft, baseline)) return;
    setBaseline(serverDraft);
    if (!dirty) {
      setDraft(serverDraft);
      if (serverDraft.notes !== "") setNoting(true);
    }
  }, [serverDraft, baseline, dirty]);

  const send = useMutation({
    mutationFn: (d: Draft) => clients.questions.answerQuestion({ id: q.id, answer: draftAnswer(d) }),
    onSuccess: (_data, d) => {
      const saved: Draft = {
        picked: d.picked,
        ownWords: d.ownWords,
        other: d.ownWords ? d.other : "",
        notes: d.notes,
      };
      setBaseline(saved);
      setDraft(saved);
    },
    onSettled: settle,
  });
  const dismiss = useMutation({
    mutationFn: () => clients.questions.dismissQuestion({ id: q.id, reason }),
    onSuccess: () => {
      setDismissing(false);
      setReason("");
    },
    onSettled: settle,
  });
  const busy = send.isPending || dismiss.isPending;
  const choose = (label: string) => {
    if (busy) return;
    const next = chooseOption(draft, label, multi);
    setDraft(next.draft);
    if (next.submit) send.mutate(next.draft);
  };
  const withPreview = options.filter((o) => o.preview);
  const shown = options.find((o) => o.label === focused && o.preview) ?? options.find((o) => draft.picked.includes(o.label) && o.preview) ?? withPreview[0];
  // A single choice is sent by clicking it; show the button for multi-select, own words, or any unsaved edits (e.g. a note on an answered question).
  const needsButton = multi || draft.ownWords || dirty;
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
            <Textarea
              rows={3}
              value={draft.other}
              onChange={(e) => setDraft({ ...draft, other: e.target.value })}
              placeholder="Your answer"
              className="bg-background/60 text-[12.5px]"
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
        <Input value={draft.notes} onChange={(e) => setDraft({ ...draft, notes: e.target.value })} placeholder="Note for the agent" className="bg-background/60 text-[12.5px]" aria-label="Note" autoFocus />
      ) : (
        <button type="button" onClick={() => setNoting(true)} className={LINK}>
          + add a note
        </button>
      )}
      <div className="flex flex-wrap items-start gap-2">
        {needsButton && (
          <Button type="submit" size="sm" disabled={!draftReady(draft) || busy} className="text-[12.5px]">
            {q.answer ? (send.isPending ? "Saving…" : "Save") : send.isPending ? "Sending…" : "Answer"}
          </Button>
        )}
        {q.state === QuestionState.DISMISSED ? null : dismissing ? (
          <>
            <Button type="button" variant="outline" size="sm" disabled={busy} onClick={() => dismiss.mutate()} className={`${dismissWidth} justify-start border-border-strong text-[12.5px]`}>
              Dismiss
            </Button>
            <Input value={reason} onChange={(e) => setReason(e.target.value)} placeholder="Why not (optional)" className="w-full bg-background/60 text-[12.5px] sm:max-w-xs" aria-label="Why you dismiss it" />
            <Button type="button" variant="ghost" size="sm" onClick={() => setDismissing(false)} className="text-[12px] text-muted-foreground">
              cancel
            </Button>
          </>
        ) : (
          <Button type="button" variant="ghost" size="sm" onClick={() => setDismissing(true)} className={`${dismissWidth} justify-start text-[12.5px] text-muted-foreground`}>
            Dismiss…
          </Button>
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
function Choice({ multi, on, busy, mark, onChoose, onFocus, children }: { multi: boolean; on: boolean; busy: boolean; mark: keyof typeof MARKS | null; onChoose: () => void; onFocus: () => void; children: ReactNode }) {
  const look = on ? "border-primary/60 bg-ember-wash" : mark ? `${MARKS[mark].row} hover:border-border-strong` : "border-border hover:border-border-strong hover:bg-muted/60";
  return (
    <div
      role={multi ? "checkbox" : "radio"}
      aria-checked={on}
      aria-disabled={busy}
      tabIndex={0}
      onClick={() => {
        if (window.getSelection()?.toString()) return;
        onChoose();
      }}
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
      clients.questions.reviseQuestion({
        id: q.id,
        prompt: {
          header: q.prompt?.header ?? "",
          options: q.prompt?.options ?? [],
          multiSelect: q.prompt?.multiSelect ?? false,
          question,
          body,
        },
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
      <Input value={question} onChange={(e) => setQuestion(e.target.value)} className="bg-background/60 text-[14px] font-semibold" aria-label="Question" autoFocus />
      <Textarea value={body} onChange={(e) => setBody(e.target.value)} rows={Math.min(14, Math.max(3, body.split("\n").length + 1))} placeholder="Body: what the answer needs to know (markdown, optional)" className="bg-background/60 font-mono text-[12px]" aria-label="Body" />
      <div className="flex flex-wrap items-center gap-2">
        <Button type="submit" size="sm" disabled={question.trim() === "" || save.isPending} className="text-[12.5px]">
          {save.isPending ? "Saving…" : "Save"}
        </Button>
        <Button type="button" variant="ghost" size="sm" onClick={onDone} className="text-[12px] text-muted-foreground">
          cancel
        </Button>
        {save.error && <span className="font-mono text-[11px] text-critical">{String(save.error)}</span>}
      </div>
    </form>
  );
}

/** Returning a question to the inbox, its response cleared. */
function ReopenButton({ q }: { q: Question }) {
  const settle = useSettle();
  const reopen = useMutation({ mutationFn: () => clients.questions.reopenQuestion({ id: q.id }), onSettled: settle });
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
function OutboxLine({ q, now }: { q: Question; now: number }) {
  const responded = q.answered ?? q.dismissed;
  return (
    <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1 text-[11px] text-muted-foreground">
      {responded && <StampLine st={responded} now={now} verb={q.dismissed ? "dismissed" : "answered"} />}
      {q.dismissed?.reason && <span className="text-foreground">{q.dismissed.reason}</span>}
      <span className="font-mono text-frost">waiting for an agent to read it; change it until then</span>
      <ReopenButton q={q} />
    </div>
  );
}

/** What became of a handled or deleted question: the user's response, and who acted on it or deleted it. */
function Response({ q, now }: { q: Question; now: number }) {
  const responded = q.answered ?? q.dismissed;
  return (
    <div className="space-y-2 rounded-lg border border-border bg-muted/40 px-3 py-2.5 text-[12.5px]">
      {responded && (
        <div className="space-y-1.5">
          <div className="text-[11px] text-muted-foreground">
            <StampLine st={responded} now={now} verb={q.dismissed ? "dismissed" : "answered"} />
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
          <StampLine st={q.handled} now={now} verb="handled" />
          <ReopenButton q={q} />
        </div>
      )}
      {q.deleted && (
        <div className="text-[11px] text-muted-foreground">
          <StampLine st={q.deleted} now={now} verb="deleted" />
          {q.deleted.reason && <span className="text-foreground">: {q.deleted.reason}</span>}
        </div>
      )}
    </div>
  );
}

/** Every change to the question, loaded when opened. */
function History({ q, now }: { q: Question; now: number }) {
  const [open, setOpen] = useState(false);
  const history = useQuery({ queryKey: ["question-history", q.id, String(q.updatedAt), String(q.rank)], queryFn: async () => (await clients.questions.getQuestion({ id: q.id })).history, enabled: open });
  return (
    <Collapsible open={open} onOpenChange={setOpen}>
      <CollapsibleTrigger className="group inline-flex cursor-pointer items-center gap-1 font-mono text-[11px] text-faint hover:text-foreground">
        <ChevronRightIcon className="size-3 transition-transform group-data-panel-open:rotate-90" />
        <span>
          history{q.edited && <span> · edited by {who(q.edited)}</span>}
        </span>
      </CollapsibleTrigger>
      <CollapsibleContent>
        {history.isPending && <div className="mt-2 font-mono text-[11px] text-faint">loading history…</div>}
        {history.isError && <div className="mt-2 font-mono text-[11px] text-critical">{String(history.error)}</div>}
        {history.data && (
          <ol className="mt-2 space-y-1.5 border-l border-border pl-3">
            {history.data.map((c) => (
              <li key={String(c.seq)} className="text-[11.5px] text-muted-foreground">
                <span className="font-mono text-[11px] text-foreground">{c.what}</span> {c.by && <StampLine st={c.by} now={now} verb="" />}
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
      </CollapsibleContent>
    </Collapsible>
  );
}
