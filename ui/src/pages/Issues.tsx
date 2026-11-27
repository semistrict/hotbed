import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams, useSearch } from "@tanstack/react-router";
import { useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { clients, IssueState, type Issue } from "../api";
import { Markdown } from "../components/Markdown";
import { Card, Empty } from "../components/Panel";
import { ProjectTag } from "../components/ProjectTag";
import { StampLine } from "../components/StampLine";
import { PageTitle } from "../components/PageTitle";

export type IssuesTab = "open" | "archived" | "all";

export interface IssuesSearch {
  tab?: IssuesTab;
}

const TABS: { tab: IssuesTab; label: string; empty: string }[] = [
  { tab: "open", label: "Open", empty: "No open issues. Agents report one with hotbed issue report." },
  { tab: "archived", label: "Archived", empty: "No issue has been archived." },
  { tab: "all", label: "All", empty: "No issues yet." },
];

/** What agents and people reported wrong with hotbed itself, most +1'd first. */
export function IssuesPage() {
  const search = useSearch({ from: "/issues" });
  const navigate = useNavigate({ from: "/issues" });
  const tab = search.tab ?? "open";
  const list = useQuery({ queryKey: ["issues", tab], queryFn: async () => (await clients.issues.listIssues({ archived: tab === "archived", all: tab === "all" })).issues, refetchInterval: 5000, placeholderData: keepPreviousData });
  const now = Date.now();
  return (
    <div className="space-y-6">
      <PageTitle
        title="Issues"
        sub="Problems agents and people hit with hotbed itself: each reported once, +1'd by everyone else who hits it. Archive one once it is dealt with."
      />
      <Tabs value={tab} onValueChange={(v) => navigate({ search: { tab: v as IssuesTab } })}>
        <TabsList aria-label="Issues" className="border border-border bg-card/70">
          {TABS.map((t) => (
            <TabsTrigger key={t.tab} value={t.tab} className="px-3 text-[12.5px]">
              {t.label}
            </TabsTrigger>
          ))}
        </TabsList>
      </Tabs>
      {list.isError && <div className="font-mono text-[11.5px] text-critical">Could not list the issues: {String(list.error)}</div>}
      {list.data && list.data.length === 0 ? (
        <Card>
          <Empty>{TABS.find((t) => t.tab === tab)?.empty}</Empty>
        </Card>
      ) : (
        <ol className="space-y-4">
          {list.data?.map((issue) => (
            <li key={issue.id}>
              <IssueCard issue={issue} now={now} />
            </li>
          ))}
        </ol>
      )}
    </div>
  );
}

/** One issue on a page of its own: where a link an agent sends leads. */
export function IssueDetailPage() {
  const { id } = useParams({ from: "/issues/$id" });
  const q = useQuery({ queryKey: ["issues", "one", id], queryFn: () => clients.issues.getIssue({ id: Number(id) }), refetchInterval: 5000 });
  return (
    <div className="space-y-6">
      <PageTitle
        title={`Issue #${id}`}
        sub="A problem with hotbed itself, as reported, and everyone who hit it."
        right={
          <Link to="/issues" className="inline-block rounded-lg border border-border px-3 py-1.5 font-mono text-[11.5px] text-muted-foreground hover:border-border-strong hover:text-foreground">
            ← all issues
          </Link>
        }
      />
      {q.isError && <div className="font-mono text-[11.5px] text-critical">Could not load issue #{id}: {String(q.error)}</div>}
      {q.data && <IssueCard issue={q.data} now={Date.now()} />}
    </div>
  );
}

function IssueCard({ issue, now }: { issue: Issue; now: number }) {
  const qc = useQueryClient();
  const [reason, setReason] = useState("");
  const settle = () => qc.invalidateQueries({ queryKey: ["issues"] }).then(() => qc.invalidateQueries({ queryKey: ["snapshot"] }));
  const archive = useMutation({ mutationFn: () => clients.issues.archiveIssue({ id: issue.id, reason: reason.trim() }), onSuccess: settle });
  const reopen = useMutation({ mutationFn: () => clients.issues.reopenIssue({ id: issue.id }), onSuccess: settle });
  const archived = issue.state === IssueState.ARCHIVED;
  const failure = archive.error ?? reopen.error;
  return (
    <Card className={archived ? "opacity-75" : ""}>
      <div className="flex gap-3">
        <div className="flex w-12 shrink-0 flex-col items-center self-start rounded-lg border border-border bg-background/50 py-1.5" title={`${issue.votes} hit it`}>
          <span className="num font-mono text-[15px] font-semibold text-foreground">+{issue.votes}</span>
        </div>
        <div className="min-w-0 flex-1 space-y-2.5">
          <header className="flex flex-wrap items-baseline gap-x-2.5 gap-y-1">
            <Link to="/issues/$id" params={{ id: String(issue.id) }} className="font-mono text-[10.5px] text-faint hover:text-foreground">
              #{issue.id}
            </Link>
            {issue.project && <ProjectTag name={issue.project} />}
            <span className={`font-mono text-[10.5px] ${archived ? "text-faint" : "text-primary"}`}>{archived ? "archived" : "open"}</span>
            {issue.reported && (
              <span className="text-[11px] text-muted-foreground sm:ml-auto">
                <StampLine st={issue.reported} now={now} verb="reported" />
              </span>
            )}
          </header>
          <h2 className="text-[15px] leading-snug font-semibold text-pretty text-foreground">{issue.title}</h2>
          {issue.body && <Markdown className="text-[13px] leading-relaxed text-foreground/85">{issue.body}</Markdown>}
          {issue.upvotes.length > 0 && (
            <ul className="space-y-1 border-t border-border pt-2">
              {issue.upvotes.map((u, i) => (
                <li key={i} className="flex flex-wrap items-baseline gap-1.5 text-[12px]">
                  <span className="font-mono text-[11px] text-primary">+1</span>
                  {u.by && (
                    <span className="text-[11px] text-muted-foreground">
                      <StampLine st={u.by} now={now} />
                    </span>
                  )}
                  {u.comment && <span className="text-foreground/85">{u.comment}</span>}
                </li>
              ))}
            </ul>
          )}
          {archived ? (
            <div className="flex flex-wrap items-center gap-2 border-t border-border pt-2 text-[12px] text-muted-foreground">
              <span>
                {issue.archived && <StampLine st={issue.archived} now={now} verb="archived" />}
                {issue.archived?.reason ? `: ${issue.archived.reason}` : ""}
              </span>
              <Button type="button" variant="outline" size="xs" className="ml-auto font-mono text-[11px]" disabled={reopen.isPending} onClick={() => reopen.mutate()}>
                Reopen
              </Button>
            </div>
          ) : (
            <form
              className="flex flex-wrap items-center gap-2 border-t border-border pt-2"
              onSubmit={(e) => {
                e.preventDefault();
                archive.mutate();
              }}
            >
              <Input
                value={reason}
                onChange={(e) => setReason(e.target.value)}
                placeholder="why: fixed in …, or won't fix because …"
                aria-label={`Why archive issue ${issue.id}`}
                className="h-7 min-w-0 flex-1 bg-background/40 font-mono text-[11.5px] placeholder:text-faint"
              />
              <Button type="submit" variant="outline" size="xs" className="font-mono text-[11px]" disabled={archive.isPending}>
                Archive
              </Button>
            </form>
          )}
          {failure && <div className="font-mono text-[11px] text-critical">{String(failure)}</div>}
        </div>
      </div>
    </Card>
  );
}
