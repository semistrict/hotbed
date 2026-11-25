import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useParams } from "@tanstack/react-router";
import { ChevronRightIcon } from "lucide-react";
import { useState } from "react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Textarea } from "@/components/ui/textarea";
import {
  api,
  FindingDecision,
  FindingSeverity,
  FindingVerdict,
  QuickResponse,
  ReviewState,
  type Finding,
  type Location,
  type Review,
  type ReviewEvent,
  type Snapshot,
  type Stamp,
} from "../api";
import { Markdown } from "../components/Markdown";
import { ProjectTag } from "../components/ProjectTag";
import { SessionLink } from "../components/SessionLink";
import { Card, Empty } from "../components/Panel";
import { useSnapshot } from "../lib/data";
import { ago } from "../lib/format";
import { remoteName } from "../lib/projects";
import { bySeverity, commitUrl, ended, handled, numbered, QUICKS, quickLabel, SEVERITIES, short, thread, where } from "../lib/reviews";
import { REVIEW_STATE, SeverityCounts } from "./Reviews";

export function ReviewPage() {
  const { id } = useParams({ from: "/reviews/$id" });
  const { data: s } = useSnapshot();
  const review = useQuery({ queryKey: ["review", Number(id)], queryFn: () => api.review(Number(id)), refetchInterval: 2000 });
  if (!s) return null;
  if (review.isError) return <div className="rounded-xl border border-critical/40 bg-card p-6 font-mono text-[12px] text-critical">{String(review.error)}</div>;
  if (!review.data) return <div className="py-24 text-center font-mono text-[12px] text-muted-foreground">loading review {id}…</div>;
  return <ReviewView s={s} r={review.data} />;
}

function ReviewView({ s, r }: { s: Snapshot; r: Review }) {
  const now = Number(s.now);
  const groups = bySeverity(r.findings);
  const state = REVIEW_STATE[r.state] ?? REVIEW_STATE[ReviewState.REQUESTED];
  return (
    <div className="space-y-6">
      <nav className="rise font-mono text-[11px] text-muted-foreground">
        <Link to="/reviews" className="hover:text-primary">
          Reviews
        </Link>{" "}
        / #{r.id}
      </nav>
      <header className="rise space-y-2">
        <div className="flex flex-wrap items-center gap-3">
          <ProjectTag name={r.project} />
          <span className={`font-mono text-[11px] ${state.className}`}>{state.text}</span>
          <SeverityCounts r={r} />
        </div>
        <h1 className="display text-[26px] leading-tight font-semibold text-balance sm:text-[34px] lg:text-[38px]">{r.title}</h1>
      </header>
      <Card>
        <dl className="grid gap-x-8 gap-y-3 text-[12px] sm:grid-cols-2 lg:grid-cols-4">
          <Fact label="version">
            <span className="font-mono" title={`${r.base || "(whole tree)"}..${r.head}`}>
              {r.repo && <span className="text-foreground">{remoteName(r.repo)} </span>}
              {r.branch && <span className="text-muted-foreground">{r.branch} · </span>}
              {r.base ? (
                <>
                  {short(r.base)}..{short(r.head)}
                </>
              ) : (
                <>{short(r.head)}, the whole tree</>
              )}
            </span>
            {r.snapshotOf && <div className="mt-0.5 text-[11px] text-warning">uncommitted changes on {short(r.snapshotOf)}, as a snapshot commit</div>}
          </Fact>
          <Fact label="asked by">
            {r.requested ? <StampLine s={s} st={r.requested} now={now} /> : <span className="text-faint">—</span>}
          </Fact>
          <Fact label="reviewer">{r.started ? <StampLine s={s} st={r.started} now={now} /> : <span className="text-faint">none yet</span>}</Fact>
          <Fact label="model">{r.model ? <span className="font-mono">{r.model}</span> : <span className="text-faint">{r.started ? "unknown" : "none yet"}</span>}</Fact>
          <Fact label={r.state === ReviewState.DONE ? "finished" : r.state === ReviewState.FAILED ? "failed" : r.state === ReviewState.CANCELLED ? "cancelled" : "ended"}>
            {r.ended ? <StampLine s={s} st={r.ended} now={now} /> : <span className="text-faint">not yet</span>}
          </Fact>
        </dl>
        {r.instructions && <Prose label="asked to look at" text={r.instructions} />}
        {r.summary && <Prose label={r.state === ReviewState.DONE ? "the reviewer's summary" : `why it ${r.state === ReviewState.FAILED ? "failed" : "was cancelled"}`} text={r.summary} />}
        {r.report && <Report text={r.report} />}
      </Card>

      {r.findings.length === 0 ? (
        <Card>
          <Empty>{r.state === ReviewState.DONE ? "The reviewer found nothing." : ended(r.state) ? "No findings." : "No findings yet; the page follows the review as it goes."}</Empty>
        </Card>
      ) : (
        groups.map((g) => {
          const sev = SEVERITIES.find((x) => x.severity === g.severity);
          return (
            <section key={g.severity} className="space-y-3">
              <h2 className="rise flex items-baseline gap-2">
                <span className={`display text-[22px] font-medium ${SEVERITY_TONE[g.severity]}`}>{sev?.label}</span>
                <span className="font-mono text-[11px] text-muted-foreground">
                  {g.findings.length} · {sev?.hint}
                </span>
              </h2>
              {g.findings.map((f, i) => (
                <FindingCard key={f.id} s={s} r={r} f={f} now={now} delay={40 * Math.min(i, 6)} />
              ))}
            </section>
          );
        })
      )}

      <Card title="On the review as a whole" sub="Comments for the agent that asked for it">
        <Thread s={s} events={thread(r.events, null)} now={now} />
        <Respond reviewId={r.id} findingId={null} />
      </Card>
      <Timeline s={s} r={r} now={now} />
    </div>
  );
}

const SEVERITY_TONE: Record<FindingSeverity, string> = {
  [FindingSeverity.UNSPECIFIED]: "text-muted-foreground",
  [FindingSeverity.IMPORTANT]: "text-critical",
  [FindingSeverity.PRE_EXISTING]: "text-warning",
  [FindingSeverity.NIT]: "text-muted-foreground",
};

function Fact({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <dt className="eyebrow">{label}</dt>
      <dd className="mt-1 text-muted-foreground">{children}</dd>
    </div>
  );
}

function Prose({ label, text }: { label: string; text: string }) {
  return (
    <div className="mt-4 border-t border-border pt-3">
      <div className="eyebrow">{label}</div>
      <p className="mt-1.5 text-[13px] leading-relaxed whitespace-pre-wrap text-foreground">{text}</p>
    </div>
  );
}

/** The reviewer's whole report, which its findings came from: folded, as the findings below are what to act on. */
function Report({ text }: { text: string }) {
  return (
    <Collapsible className="mt-4 border-t border-border pt-3">
      <CollapsibleTrigger className="eyebrow group inline-flex cursor-pointer items-center gap-1 hover:text-foreground">
        <ChevronRightIcon className="size-3 transition-transform group-data-panel-open:rotate-90" />
        <span>the reviewer's report</span>
      </CollapsibleTrigger>
      <CollapsibleContent>
        <Markdown className="mt-1.5 text-[13px] leading-relaxed text-foreground">{text}</Markdown>
      </CollapsibleContent>
    </Collapsible>
  );
}

/** "brave-otter on asahimbp, 5m ago", or "you". */
function StampLine({ s, st, now }: { s: Snapshot; st: Stamp; now: number }) {
  const listed = st.session !== "" && s.sessions.some((x) => x.id === st.session);
  return (
    <span title={new Date(Number(st.at)).toLocaleString()}>
      {st.session === "" ? <span className="text-foreground">you</span> : listed ? <SessionLink id={st.session} /> : <span className="font-mono text-[11px]">{st.session}</span>}
      {st.host && <span className="text-faint"> on {st.host}</span>}, {ago(st.at, now)}
    </span>
  );
}

const DECISION: Record<FindingDecision, { text: string; className: string }> = {
  [FindingDecision.UNSPECIFIED]: { text: "undecided", className: "text-muted-foreground border-border" },
  [FindingDecision.OPEN]: { text: "undecided", className: "text-muted-foreground border-border" },
  [FindingDecision.ACCEPTED]: { text: "✓ accepted", className: "text-good-ink border-good/40" },
  [FindingDecision.DISMISSED]: { text: "⊘ dismissed", className: "text-faint border-border" },
  [FindingDecision.FIXED]: { text: "✓ fixed", className: "text-good-ink border-good/40 bg-good/10" },
  [FindingDecision.INVALID]: { text: "✗ invalid", className: "text-faint border-border" },
};

function FindingCard({ s, r, f, now, delay }: { s: Snapshot; r: Review; f: Finding; now: number; delay: number }) {
  const decision = DECISION[f.decision] ?? DECISION[FindingDecision.OPEN];
  return (
    <Card delay={delay} className={f.decision === FindingDecision.DISMISSED || f.decision === FindingDecision.INVALID ? "opacity-75" : ""}>
      <div className="space-y-3" id={`finding-${f.id}`}>
        <header className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
          <span className="font-mono text-[10.5px] text-faint">#{f.id}</span>
          <h3 className="text-[15px] font-semibold text-foreground">{f.title}</h3>
          {f.category && <span className="font-mono text-[10px] tracking-wider text-faint uppercase">{f.category}</span>}
          {f.verdict !== FindingVerdict.UNSPECIFIED && (
            <span className={`font-mono text-[10.5px] ${f.verdict === FindingVerdict.CONFIRMED ? "text-critical" : "text-warning"}`}>
              {f.verdict === FindingVerdict.CONFIRMED ? "confirmed" : "plausible"}
            </span>
          )}
          <Badge
            variant="outline"
            className={`h-auto max-w-full rounded-md px-1.5 py-0.5 text-left font-mono text-[10.5px] font-normal whitespace-normal sm:ml-auto ${decision.className}`}
            title={f.decided ? `${f.decided.session || "you"}: ${f.decided.reason}` : "The agent that asked has not decided"}
          >
            {decision.text}
            {f.decided?.reason && <span className="text-muted-foreground">: {f.decided.reason}</span>}
          </Badge>
        </header>
        <p className="text-[13px] leading-relaxed whitespace-pre-wrap text-foreground">{f.summary}</p>
        {f.failureScenario && (
          <div className="rounded-lg border-l-2 border-critical/50 bg-muted/40 px-3 py-2 text-[12.5px]">
            <span className="eyebrow mr-2">when it fails</span>
            <span className="whitespace-pre-wrap text-foreground">{f.failureScenario}</span>
          </div>
        )}
        {f.locations.map((l, i) => (
          <Excerpt key={i} l={l} />
        ))}
        {f.fix && <FixNote s={s} r={r} f={f} now={now} />}
        {f.evidence && <Evidence s={s} f={f} now={now} />}
        <Thread s={s} events={thread(r.events, f.id)} now={now} />
        {handled(f) ? null : <Respond reviewId={r.id} findingId={f.id} latest={f.quick} />}
      </div>
    </Card>
  );
}

/** How a finding was fixed: the commit, where, and the fixing agent's notes. It is handled: nothing more to say to it. */
function FixNote({ s, r, f, now }: { s: Snapshot; r: Review; f: Finding; now: number }) {
  if (!f.fix) return null;
  const url = commitUrl(r.repo, f.fix.commit);
  return (
    <div className="space-y-1.5 rounded-lg border border-good/40 bg-good/5 px-3 py-2 text-[12.5px]">
      <div className="flex flex-wrap items-baseline gap-x-2">
        <span className="eyebrow">fixed in</span>
        {url ? (
          <a href={url} target="_blank" rel="noreferrer" className="font-mono text-frost hover:underline" title={f.fix.commit}>
            {short(f.fix.commit)}
          </a>
        ) : (
          <span className="font-mono text-foreground" title={f.fix.commit}>
            {short(f.fix.commit)}
          </span>
        )}
        {f.decided && (
          <span className="text-muted-foreground">
            by <StampLine s={s} st={f.decided} now={now} />
          </span>
        )}
      </div>
      {f.fix.locations.length > 0 && (
        <ul className="flex flex-wrap gap-x-3 font-mono text-[11px] text-foreground">
          {f.fix.locations.map((l, i) => (
            <li key={i}>{where(l)}</li>
          ))}
        </ul>
      )}
      {f.fix.notes && <Markdown className="text-[12.5px] leading-relaxed text-foreground">{f.fix.notes}</Markdown>}
      <div className="font-mono text-[11px] text-faint">Handled: it takes no more responses.</div>
    </div>
  );
}

/** Why a finding is not real, as the agent that marked it invalid shows. The user may still dispute it. */
function Evidence({ s, f, now }: { s: Snapshot; f: Finding; now: number }) {
  return (
    <div className="space-y-1.5 rounded-lg border border-border bg-muted/40 px-3 py-2 text-[12.5px]">
      <div className="flex flex-wrap items-baseline gap-x-2">
        <span className="eyebrow">why it is not real</span>
        {f.decided && (
          <span className="text-muted-foreground">
            <StampLine s={s} st={f.decided} now={now} />
          </span>
        )}
      </div>
      <Markdown className="text-[12.5px] leading-relaxed text-foreground">{f.evidence}</Markdown>
    </div>
  );
}

function Excerpt({ l }: { l: Location }) {
  const lines = numbered(l);
  return (
    <figure className="overflow-hidden rounded-lg border border-border">
      <figcaption className="break-all border-b border-border bg-muted/60 px-3 py-1 font-mono text-[11px] text-foreground">{where(l)}</figcaption>
      {lines.length > 0 && (
        <pre className="overflow-x-auto bg-background/50 py-1.5 font-mono text-[11.5px] leading-relaxed">
          {lines.map((line, i) => (
            <div key={i} className="flex">
              {line.n !== null && <span className="w-9 shrink-0 pr-2.5 text-right text-faint select-none sm:w-12 sm:pr-3">{line.n}</span>}
              <span className={`pr-3 whitespace-pre ${line.n === null ? "pl-3" : ""}`}>{line.text}</span>
            </div>
          ))}
        </pre>
      )}
    </figure>
  );
}

function Thread({ s, events, now }: { s: Snapshot; events: ReviewEvent[]; now: number }) {
  if (events.length === 0) return null;
  return (
    <ol className="space-y-2 border-l-2 border-border pl-3">
      {events.map((e) => (
        <li key={String(e.seq)} className="text-[12.5px]">
          {e.by && (
            <div className="text-[11px] text-muted-foreground">
              <StampLine s={s} st={e.by} now={now} />
            </div>
          )}
          {e.quick !== QuickResponse.UNSPECIFIED && (
            <span className="mr-2 rounded-md bg-ember-wash px-1.5 py-0.5 font-mono text-[10.5px] text-primary">{quickLabel(e.quick)}</span>
          )}
          {e.text && <span className="whitespace-pre-wrap text-foreground">{e.text}</span>}
        </li>
      ))}
    </ol>
  );
}

/**
 * The user's response: a quick response, sent at once with whatever comment
 * is written, or a comment alone. On the review as a whole, only comments.
 */
function Respond({ reviewId, findingId, latest = QuickResponse.UNSPECIFIED }: { reviewId: number; findingId: number | null; latest?: QuickResponse }) {
  const qc = useQueryClient();
  const [text, setText] = useState("");
  const send = useMutation({
    mutationFn: (quick: QuickResponse) => api.respond(reviewId, findingId, quick, text),
    onSuccess: () => setText(""),
    onSettled: () => Promise.all([qc.invalidateQueries({ queryKey: ["review", reviewId] }), qc.invalidateQueries({ queryKey: ["reviews"] })]),
  });
  return (
    <div className="space-y-2">
      <Textarea
        rows={2}
        value={text}
        onChange={(e) => setText(e.target.value)}
        placeholder={findingId === null ? "Comment on the review" : "Comment for the agent (optional with a quick response)"}
        aria-label={findingId === null ? "Comment on the review" : `Comment on finding ${findingId}`}
        className="min-h-14 bg-background/60 text-[12.5px] placeholder:text-faint"
      />
      <div className="flex flex-wrap items-center gap-2">
        {findingId !== null &&
          QUICKS.map((q) => (
            <Button
              key={q.quick}
              type="button"
              variant="outline"
              size="sm"
              disabled={send.isPending}
              onClick={() => send.mutate(q.quick)}
              title={q.hint}
              aria-pressed={latest === q.quick}
              className={`text-[12px] ${latest === q.quick ? "border-primary/60 bg-ember-wash text-primary hover:bg-ember-wash" : "border-border text-foreground"}`}
            >
              {q.label}
            </Button>
          ))}
        <Button
          type="button"
          size="sm"
          disabled={text.trim() === "" || send.isPending}
          onClick={() => send.mutate(QuickResponse.UNSPECIFIED)}
          className="text-[12.5px]"
        >
          {send.isPending ? "Sending…" : "Comment"}
        </Button>
        {send.error && <span className="font-mono text-[11px] text-critical">{String(send.error)}</span>}
      </div>
    </div>
  );
}

/** Every event, oldest first. */
function Timeline({ s, r, now }: { s: Snapshot; r: Review; now: number }) {
  return (
    <Collapsible>
      <CollapsibleTrigger className="group inline-flex cursor-pointer items-center gap-1 font-mono text-[11px] text-faint hover:text-foreground">
        <ChevronRightIcon className="size-3 transition-transform group-data-panel-open:rotate-90" />
        <span>history: {r.events.length} events</span>
      </CollapsibleTrigger>
      <CollapsibleContent>
        <ol className="mt-2 space-y-1 border-l border-border pl-3">
          {r.events.map((e) => (
            <li key={String(e.seq)} className="text-[11.5px] text-muted-foreground">
              <span className="font-mono text-[11px] text-foreground">{e.what}</span>
              {e.findingId !== 0 && <span className="font-mono text-faint"> #{e.findingId}</span>} {e.by && <>by <StampLine s={s} st={e.by} now={now} /></>}
              {e.quick !== QuickResponse.UNSPECIFIED && <span>: {quickLabel(e.quick)}</span>}
              {e.text && <span className="text-foreground">: {e.text}</span>}
            </li>
          ))}
        </ol>
      </CollapsibleContent>
    </Collapsible>
  );
}
