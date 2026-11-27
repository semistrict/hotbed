import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { clients, projectScope, ReviewState, type Review, type Snapshot } from "../api";
import { SessionLink } from "../components/SessionLink";
import { ProjectTag } from "../components/ProjectTag";
import { Card, Empty, StatTile } from "../components/Panel";
import { ago } from "../lib/format";
import { useProject, useScopedSnapshot } from "../lib/project";
import { remoteName } from "../lib/projects";
import { reviewerModels, versionLine } from "../lib/reviews";
import { PageTitle } from "../components/PageTitle";

export function ReviewsPage() {
  const { data: s } = useScopedSnapshot();
  if (!s) return null;
  return <Reviews s={s} />;
}

function Reviews({ s }: { s: Snapshot }) {
  const [project] = useProject();
  const list = useQuery({
    queryKey: ["reviews", project],
    queryFn: async () => (await clients.reviews.listReviews({ scope: projectScope(project) })).reviews,
    refetchInterval: 2000,
    placeholderData: (prev, prevQuery) => (prevQuery?.queryKey[1] === project ? prev : undefined),
  });
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
        {!list.data && list.isPending ? (
          <div className="py-12 text-center font-mono text-[12px] text-muted-foreground">loading reviews…</div>
        ) : list.data && reviews.length === 0 ? (
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
    <>
      <div className="space-y-3 lg:hidden">
        {reviews.map((r) => {
          const st = REVIEW_STATE[r.state] ?? REVIEW_STATE[ReviewState.REQUESTED];
          return (
            <div key={r.id} className="rounded-xl border border-border/70 bg-background/40 p-3 font-mono text-[11px]">
              <div className="flex flex-wrap items-center gap-2">
                <span className="text-faint">#{r.id}</span>
                {showProject && <ProjectTag name={r.project} />}
                <span className={st.className}>{st.text}</span>
                {r.open > 0 && r.state === ReviewState.DONE && <span className="ml-auto text-primary">{r.open} open</span>}
              </div>
              <Link to="/reviews/$id" params={{ id: String(r.id) }} className="mt-1.5 block font-sans text-[14.5px] leading-snug font-medium text-foreground hover:text-primary">
                {r.title}
              </Link>
              <div className="mt-1 break-words text-[10.5px] text-muted-foreground" title={`${r.base}..${r.head}`}>
                {r.repo && <span className="text-foreground">{remoteName(r.repo)} </span>}
                {r.branch && <span>{r.branch} · </span>}
                {versionLine(r)}
                {r.snapshotOf && <span className="text-warning-ink"> · uncommitted</span>}
              </div>
              <div className="mt-2 flex flex-wrap items-center justify-between gap-2 border-t border-border/60 pt-2 text-[10.5px]">
                <SeverityCounts r={r} />
                <span className="text-muted-foreground">{r.responses} resp</span>
              </div>
              <div className="mt-1.5 text-[10.5px] text-muted-foreground">
                {r.requested && (
                  <span>
                    impl: {r.implementerModel || r.implementer?.model || "unknown"} <SessionLink id={r.implementer?.session || r.requested.session} empty={null} />{" "}
                    <span className="text-faint">{ago(r.requested.at, now)}</span>
                  </span>
                )}
                <div className="text-faint">
                  {(() => {
                    const models = reviewerModels(r);
                    return models.length > 0 ? `rev: ${models.join(", ")}` : "no reviewer yet";
                  })()}
                </div>
              </div>
            </div>
          );
        })}
      </div>

      <div className="hidden lg:block">
        <Table className="text-[12px]">
          <TableHeader className="eyebrow">
            <TableRow className="border-border hover:bg-transparent">
              {["review", ...(showProject ? ["project"] : []), "state", "findings", "open", "responses", "implementer · reviewers"].map((h) => (
                <TableHead key={h} className="h-auto px-2 py-2 font-normal text-muted-foreground">
                  {h}
                </TableHead>
              ))}
            </TableRow>
          </TableHeader>
          <TableBody className="font-mono text-[11px]">
            {reviews.map((r) => {
              const st = REVIEW_STATE[r.state] ?? REVIEW_STATE[ReviewState.REQUESTED];
              const models = reviewerModels(r);
              return (
                <TableRow key={r.id} className="border-border/60 align-top hover:bg-muted/60">
                  <TableCell className="px-2 py-2.5 whitespace-normal">
                    <Link to="/reviews/$id" params={{ id: String(r.id) }} className="font-sans text-[14px] font-medium text-foreground hover:text-primary">
                      {r.title}
                    </Link>
                    <span className="ml-2 text-faint">#{r.id}</span>
                    <div className="mt-0.5 text-[10.5px] text-muted-foreground" title={`${r.base}..${r.head}`}>
                      {r.repo && <span className="text-foreground">{remoteName(r.repo)} </span>}
                      {r.branch && <span>{r.branch} · </span>}
                      {versionLine(r)}
                      {r.snapshotOf && <span className="text-warning-ink"> · uncommitted</span>}
                    </div>
                  </TableCell>
                  {showProject && (
                    <TableCell className="px-2 py-2.5">
                      <ProjectTag name={r.project} />
                    </TableCell>
                  )}
                  <TableCell className={`px-2 py-2.5 ${st.className}`}>{st.text}</TableCell>
                  <TableCell className="px-2 py-2.5">
                    <SeverityCounts r={r} stacked />
                  </TableCell>
                  <TableCell className={`px-2 py-2.5 ${r.open > 0 && r.state === ReviewState.DONE ? "text-primary" : "text-faint"}`}>{r.open}</TableCell>
                  <TableCell className="px-2 py-2.5 text-muted-foreground">{r.responses}</TableCell>
                  <TableCell className="px-2 py-2.5 text-muted-foreground">
                    {r.requested && (
                      <div>
                        <span className="text-foreground">{r.implementerModel || r.implementer?.model || "unknown"}</span>{" "}
                        <SessionLink id={r.implementer?.session || r.requested.session} empty={null} />{" "}
                        <span className="text-faint">{ago(r.requested.at, now)}</span>
                      </div>
                    )}
                    <div className="text-faint">
                      {models.length > 0 ? `→ ${models.join(", ")}${r.reviewers.length > 1 ? ` (${r.reviewers.length})` : ""}` : "no reviewer yet"}
                    </div>
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      </div>
    </>
  );
}

export function SeverityCounts({ r, stacked = false }: { r: Review; stacked?: boolean }) {
  const { important, preExisting, nits } = r;
  if (important + preExisting + nits === 0) return <span className="text-faint">{r.state === ReviewState.DONE ? "none" : "—"}</span>;
  return (
    <span className={stacked ? "flex flex-col" : "flex gap-2 font-mono text-[11px]"}>
      {important > 0 && <span className="text-critical">{important} important</span>}
      {preExisting > 0 && <span className="text-warning-ink">{preExisting} pre-existing</span>}
      {nits > 0 && <span className="text-muted-foreground">{nits} nit{nits > 1 ? "s" : ""}</span>}
    </span>
  );
}
