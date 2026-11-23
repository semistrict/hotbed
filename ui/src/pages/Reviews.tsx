import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { api, ReviewState, type Review, type Snapshot } from "../api";
import { ProjectTag } from "../components/ProjectTag";
import { Card, Empty, StatTile } from "../components/ui/Card";
import { ago } from "../lib/format";
import { useProject, useScopedSnapshot } from "../lib/project";
import { remoteName } from "../lib/projects";
import { versionLine } from "../lib/reviews";
import { PageTitle } from "./Nodes";

export function ReviewsPage() {
  const { data: s } = useScopedSnapshot();
  if (!s) return null;
  return <Reviews s={s} />;
}

function Reviews({ s }: { s: Snapshot }) {
  const [project] = useProject();
  const list = useQuery({ queryKey: ["reviews", project], queryFn: () => api.reviews(project), refetchInterval: 2000, placeholderData: keepPreviousData });
  const now = Number(s.now);
  const reviews = list.data ?? [];
  const active = reviews.filter((r) => r.state === ReviewState.REQUESTED || r.state === ReviewState.IN_PROGRESS);
  const toDecide = reviews.filter((r) => r.state === ReviewState.DONE && r.open > 0);
  const responses = reviews.reduce((n, r) => n + r.responses, 0);

  return (
    <div className="space-y-6">
      <PageTitle
        title="Reviews"
        sub={`Reviews of an exact version by reviewer agents, in ${project ?? "every project"}. The agent that asked decides each finding; respond to any of them, and it reads what you said.`}
      />
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <StatTile label="Under way" value={active.length} tone="ember" foot={`${active.filter((r) => r.state === ReviewState.REQUESTED).length} waiting for a reviewer`} />
        <StatTile label="To decide" value={toDecide.length} foot="done, with findings still open" />
        <StatTile label="Important" value={reviews.reduce((n, r) => n + r.important, 0)} foot="findings, across these reviews" />
        <StatTile label="Responses" value={responses} foot="yours and the agents'" />
      </div>
      {list.isError && <div className="rounded-xl border border-critical/40 bg-card px-4 py-2.5 font-mono text-[11.5px] text-critical">Could not list the reviews: {String(list.error)}</div>}
      <Card>
        {list.data && reviews.length === 0 ? (
          <Empty>No reviews yet. An agent asks for one with hotbed review request.</Empty>
        ) : (
          <ReviewTable reviews={reviews} now={now} showProject={project === null} />
        )}
      </Card>
    </div>
  );
}

export const REVIEW_STATE: Record<ReviewState, { text: string; className: string }> = {
  [ReviewState.UNSPECIFIED]: { text: "◌ waiting for a reviewer", className: "text-muted-foreground" },
  [ReviewState.REQUESTED]: { text: "◌ waiting for a reviewer", className: "text-muted-foreground" },
  [ReviewState.IN_PROGRESS]: { text: "● reviewing", className: "text-primary" },
  [ReviewState.DONE]: { text: "✓ done", className: "text-good-ink" },
  [ReviewState.FAILED]: { text: "✕ failed", className: "text-critical" },
  [ReviewState.CANCELLED]: { text: "⊘ cancelled", className: "text-faint" },
};

function ReviewTable({ reviews, now, showProject }: { reviews: Review[]; now: number; showProject: boolean }) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-[12px]">
        <thead className="eyebrow">
          <tr className="border-b border-border">
            {["review", ...(showProject ? ["project"] : []), "state", "findings", "open", "responses", "asked by · reviewer"].map((h) => (
              <th key={h} className="px-2 py-2 font-normal whitespace-nowrap">
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="font-mono text-[11px] [&_td]:whitespace-nowrap">
          {reviews.map((r) => {
            const st = REVIEW_STATE[r.state] ?? REVIEW_STATE[ReviewState.REQUESTED];
            return (
              <tr key={r.id} className="border-b border-border/60 align-top hover:bg-muted/60">
                <td className="min-w-[320px] px-2 py-2.5 !whitespace-normal">
                  <Link to="/reviews/$id" params={{ id: String(r.id) }} className="font-sans text-[14px] font-medium text-foreground hover:text-primary">
                    {r.title}
                  </Link>
                  <span className="ml-2 text-faint">#{r.id}</span>
                  <div className="mt-0.5 text-[10.5px] text-muted-foreground" title={`${r.base}..${r.head}`}>
                    {r.repo && <span className="text-foreground">{remoteName(r.repo)} </span>}
                    {r.branch && <span>{r.branch} · </span>}
                    {versionLine(r)}
                    {r.snapshotOf && <span className="text-warning"> · uncommitted</span>}
                  </div>
                </td>
                {showProject && (
                  <td className="px-2 py-2.5">
                    <ProjectTag name={r.project} />
                  </td>
                )}
                <td className={`px-2 py-2.5 ${st.className}`}>{st.text}</td>
                <td className="px-2 py-2.5">
                  <SeverityCounts r={r} stacked />
                </td>
                <td className={`px-2 py-2.5 ${r.open > 0 && r.state === ReviewState.DONE ? "text-primary" : "text-faint"}`}>{r.open}</td>
                <td className="px-2 py-2.5 text-muted-foreground">{r.responses}</td>
                <td className="px-2 py-2.5 text-muted-foreground">
                  {r.requested && (
                    <div>
                      {r.requested.session} <span className="text-faint">{ago(r.requested.at, now)}</span>
                    </div>
                  )}
                  <div className="text-faint">{r.started ? `→ ${r.started.session} · ${r.model || "model unknown"}` : "no reviewer yet"}</div>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

export function SeverityCounts({ r, stacked = false }: { r: Review; stacked?: boolean }) {
  const { important, preExisting, nits } = r;
  if (important + preExisting + nits === 0) return <span className="text-faint">{r.state === ReviewState.DONE ? "none" : "—"}</span>;
  return (
    <span className={stacked ? "flex flex-col" : "flex gap-2 font-mono text-[11px]"}>
      {important > 0 && <span className="text-critical">{important} important</span>}
      {preExisting > 0 && <span className="text-warning">{preExisting} pre-existing</span>}
      {nits > 0 && <span className="text-muted-foreground">{nits} nit{nits > 1 ? "s" : ""}</span>}
    </span>
  );
}
