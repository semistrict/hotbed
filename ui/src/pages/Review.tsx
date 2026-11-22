import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useParams } from "@tanstack/react-router";
import { useState } from "react";
import { api, type Finding, type Location, type Quick, type Review, type ReviewEvent, type Snapshot, type Stamp } from "../api";
import { ProjectTag } from "../components/ProjectTag";
import { SessionLink } from "../components/SessionLink";
import { Card, Empty } from "../components/ui/Card";
import { useSnapshot } from "../lib/data";
import { ago } from "../lib/format";
import { remoteName } from "../lib/projects";
import { bySeverity, ended, numbered, QUICKS, quickLabel, SEVERITIES, short, thread, where } from "../lib/reviews";
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
  const now = Date.parse(s.now);
  const groups = bySeverity(r.findings);
  const state = REVIEW_STATE[r.state];
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
        <h1 className="display text-[38px] leading-tight font-semibold text-balance">{r.title}</h1>
      </header>
      <Card>
        <dl className="grid gap-x-8 gap-y-3 text-[12px] sm:grid-cols-2 lg:grid-cols-4">
          <Fact label="version">
            <span className="font-mono" title={`${r.base ?? "(whole tree)"}..${r.head}`}>
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
            <StampLine s={s} st={r.requested} now={now} />
          </Fact>
          <Fact label="reviewer">{r.started ? <StampLine s={s} st={r.started} now={now} /> : <span className="text-faint">none yet</span>}</Fact>
          <Fact label={r.state === "done" ? "finished" : ended(r.state) ? r.state : "ended"}>
            {r.ended ? <StampLine s={s} st={r.ended} now={now} /> : <span className="text-faint">not yet</span>}
          </Fact>
        </dl>
        {r.instructions && <Prose label="asked to look at" text={r.instructions} />}
        {r.summary && <Prose label={r.state === "done" ? "the reviewer's summary" : `why it ${r.state === "failed" ? "failed" : "was cancelled"}`} text={r.summary} />}
      </Card>

      {r.findings.length === 0 ? (
        <Card>
          <Empty>{r.state === "done" ? "The reviewer found nothing." : ended(r.state) ? "No findings." : "No findings yet; the page follows the review as it goes."}</Empty>
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

const SEVERITY_TONE: Record<Finding["severity"], string> = {
  important: "text-critical",
  "pre-existing": "text-warning",
  nit: "text-muted-foreground",
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

/** "brave-otter on asahimbp, 5m ago", or "you". */
function StampLine({ s, st, now }: { s: Snapshot; st: Stamp; now: number }) {
  const listed = st.session !== null && s.sessions.some((x) => x.id === st.session);
  return (
    <span title={new Date(st.at).toLocaleString()}>
      {st.session === null ? <span className="text-foreground">you</span> : listed ? <SessionLink id={st.session} /> : <span className="font-mono text-[11px]">{st.session}</span>}
      {st.host && <span className="text-faint"> on {st.host}</span>}, {ago(st.at, now)}
    </span>
  );
}

const DECISION: Record<Finding["decision"], { text: string; className: string }> = {
  open: { text: "undecided", className: "text-muted-foreground border-border" },
  accepted: { text: "✓ accepted", className: "text-good-ink border-good/40" },
  dismissed: { text: "⊘ dismissed", className: "text-faint border-border" },
};

function FindingCard({ s, r, f, now, delay }: { s: Snapshot; r: Review; f: Finding; now: number; delay: number }) {
  const decision = DECISION[f.decision];
  return (
    <Card delay={delay} className={f.decision === "dismissed" ? "opacity-75" : ""}>
      <div className="space-y-3" id={`finding-${f.id}`}>
        <header className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
          <span className="font-mono text-[10.5px] text-faint">#{f.id}</span>
          <h3 className="text-[15px] font-semibold text-foreground">{f.title}</h3>
          {f.category && <span className="font-mono text-[10px] tracking-wider text-faint uppercase">{f.category}</span>}
          {f.verdict && <span className={`font-mono text-[10.5px] ${f.verdict === "confirmed" ? "text-critical" : "text-warning"}`}>{f.verdict}</span>}
          <span className={`ml-auto rounded-md border px-1.5 py-0.5 font-mono text-[10.5px] ${decision.className}`} title={f.decided ? `${f.decided.session ?? "you"}: ${f.decided.reason}` : "The agent that asked has not decided"}>
            {decision.text}
            {f.decided?.reason && <span className="text-muted-foreground">: {f.decided.reason}</span>}
          </span>
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
        <Thread s={s} events={thread(r.events, f.id)} now={now} />
        <Respond reviewId={r.id} findingId={f.id} latest={f.quick} />
      </div>
    </Card>
  );
}

function Excerpt({ l }: { l: Location }) {
  const lines = numbered(l);
  return (
    <figure className="overflow-hidden rounded-lg border border-border">
      <figcaption className="border-b border-border bg-muted/60 px-3 py-1 font-mono text-[11px] text-foreground">{where(l)}</figcaption>
      {lines.length > 0 && (
        <pre className="overflow-x-auto bg-background/50 py-1.5 font-mono text-[11.5px] leading-relaxed">
          {lines.map((line, i) => (
            <div key={i} className="flex">
              {line.n !== null && <span className="w-12 shrink-0 pr-3 text-right text-faint select-none">{line.n}</span>}
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
        <li key={e.seq} className="text-[12.5px]">
          <div className="text-[11px] text-muted-foreground">
            <StampLine s={s} st={e.by} now={now} />
          </div>
          {e.quick && <span className="mr-2 rounded-md bg-ember-wash px-1.5 py-0.5 font-mono text-[10.5px] text-primary">{quickLabel(e.quick)}</span>}
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
function Respond({ reviewId, findingId, latest = null }: { reviewId: number; findingId: number | null; latest?: Quick | null }) {
  const qc = useQueryClient();
  const [text, setText] = useState("");
  const send = useMutation({
    mutationFn: (quick: Quick | null) => api.respond(reviewId, findingId, quick, text),
    onSuccess: () => setText(""),
    onSettled: () => Promise.all([qc.invalidateQueries({ queryKey: ["review", reviewId] }), qc.invalidateQueries({ queryKey: ["reviews"] })]),
  });
  return (
    <div className="space-y-2">
      <textarea
        rows={2}
        value={text}
        onChange={(e) => setText(e.target.value)}
        placeholder={findingId === null ? "Comment on the review" : "Comment for the agent (optional with a quick response)"}
        aria-label={findingId === null ? "Comment on the review" : `Comment on finding ${findingId}`}
        className="w-full rounded-lg border border-border bg-background/60 px-3 py-2 text-[12.5px] placeholder:text-faint focus:border-primary/50 focus:ring-2 focus:ring-primary/20 focus:outline-none"
      />
      <div className="flex flex-wrap items-center gap-2">
        {findingId !== null &&
          QUICKS.map((q) => (
            <button
              key={q.quick}
              type="button"
              disabled={send.isPending}
              onClick={() => send.mutate(q.quick)}
              title={q.hint}
              aria-pressed={latest === q.quick}
              className={`rounded-lg border px-3 py-1.5 text-[12px] transition-colors disabled:opacity-40 ${latest === q.quick ? "border-primary/60 bg-ember-wash text-primary" : "border-border text-foreground hover:border-border-strong hover:bg-muted"}`}
            >
              {q.label}
            </button>
          ))}
        <button
          type="button"
          disabled={text.trim() === "" || send.isPending}
          onClick={() => send.mutate(null)}
          className="rounded-lg bg-primary px-3.5 py-1.5 text-[12.5px] font-medium text-primary-foreground transition-opacity hover:opacity-90 disabled:opacity-40"
        >
          {send.isPending ? "Sending…" : "Comment"}
        </button>
        {send.error && <span className="font-mono text-[11px] text-critical">{String(send.error)}</span>}
      </div>
    </div>
  );
}

/** Every event, oldest first. */
function Timeline({ s, r, now }: { s: Snapshot; r: Review; now: number }) {
  return (
    <details className="group">
      <summary className="cursor-pointer list-none font-mono text-[11px] text-faint marker:hidden hover:text-foreground">
        <span className="inline-block transition-transform group-open:rotate-90">›</span> history: {r.events.length} events
      </summary>
      <ol className="mt-2 space-y-1 border-l border-border pl-3">
        {r.events.map((e) => (
          <li key={e.seq} className="text-[11.5px] text-muted-foreground">
            <span className="font-mono text-[11px] text-foreground">{e.what}</span>
            {e.findingId !== null && <span className="font-mono text-faint"> #{e.findingId}</span>} by <StampLine s={s} st={e.by} now={now} />
            {e.quick && <span>: {quickLabel(e.quick)}</span>}
            {e.text && <span className="text-foreground">: {e.text}</span>}
          </li>
        ))}
      </ol>
    </details>
  );
}
