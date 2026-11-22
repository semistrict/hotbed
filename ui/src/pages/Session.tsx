import { Link, useParams } from "@tanstack/react-router";
import type { Session, Snapshot } from "../api";
import { FAILURE } from "../components/CommandTable";
import { Card, Empty, StatTile } from "../components/ui/Card";
import { KindTag, StateBadge } from "../components/ui/State";
import { nodeName, repoName, useSnapshot } from "../lib/data";
import { ago, duration, hhmmss, speedup } from "../lib/format";
import { idleLeftMs, sessionCommands, workspaceRepo } from "../lib/sessions";
import { NotFound } from "./NotFound";
import { PageTitle } from "./Nodes";
import { IdleLeft, SessionStateLabel } from "./Sessions";
import { WorkspaceStateLabel } from "./Workspace";

export function SessionPage() {
  const { id } = useParams({ from: "/sessions/$id" });
  const { data: s } = useSnapshot();
  if (!s) return null;
  const session = s.sessions.find((x) => x.id === id);
  if (!session) return <NotFound />;
  return <SessionView s={s} session={session} />;
}

function SessionView({ s, session }: { s: Snapshot; session: Session }) {
  const now = Date.parse(s.now);
  const rows = sessionCommands(s, session.id, workspaceRepo(s));
  const failed = rows.filter((r) => r.state === "failed").length;
  const live = rows.filter((r) => r.live).length;
  const saved = rows.reduce((n, r) => n + (!r.live && r.coldSeconds !== null ? Math.max(0, r.coldSeconds - r.seconds) : 0), 0);
  const spent = rows.reduce((n, r) => n + r.seconds, 0);
  const lasted = ((session.endedAt ? Date.parse(session.endedAt) : now) - Date.parse(session.startedAt)) / 1000;

  return (
    <div className="space-y-6">
      <PageTitle
        title={session.id}
        sub={`${session.agent} · started ${ago(session.startedAt, now)} · last active ${ago(session.lastActiveAt, now)}${session.endedAt ? ` · ended ${ago(session.endedAt, now)}: ${session.endReason}` : ""}`}
        right={<SessionStateLabel s={s} session={session} />}
      />
      {!session.endedAt && (
        <div className="rise max-w-md">
          <div className="eyebrow mb-1">abandoned in</div>
          <IdleLeft left={idleLeftMs(session, now)} />
        </div>
      )}
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-6">
        <StatTile label="Commands" value={rows.length} foot={`${live} live`} tone="ember" />
        <StatTile label="Failed" value={failed} foot={rows.length ? `${Math.round((100 * failed) / rows.length)}% of them` : "none run"} />
        <StatTile label="Ran for" value={duration(spent)} foot="all commands together" />
        <StatTile label="Saved" value={duration(saved)} tone="good" foot="against cold builds" />
        <StatTile label="Workspaces" value={session.workspaces.length} foot={`${session.workspaces.filter((w) => !w.releasedAt).length} held`} />
        <StatTile label="Lasted" value={duration(lasted)} foot={session.endedAt ? "start to end" : "so far"} />
      </div>

      <Card title="Workspaces" sub="One per repo; a workspace is a fork of its repo's built base">
        <div className="overflow-x-auto">
          <table className="w-full text-left text-[12px]">
            <thead className="eyebrow">
              <tr className="border-b border-border">
                {["workspace", "repo", "node", "state", "made", "files changed"].map((h) => (
                  <th key={h} className="px-2 py-2 font-normal whitespace-nowrap">
                    {h}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody className="num font-mono text-[11px] [&_td]:whitespace-nowrap">
              {session.workspaces.map((sw) => {
                const w = s.workspaces.find((x) => x.id === sw.id);
                return (
                  <tr key={sw.id} className="border-b border-border/60 hover:bg-muted/60">
                    <td className="px-2 py-2">
                      {w ? (
                        <Link to="/workspaces/$id" params={{ id: sw.id }} className="text-foreground hover:text-primary">
                          {sw.id}
                        </Link>
                      ) : (
                        <span className="text-faint">{sw.id}</span>
                      )}
                    </td>
                    <td className="px-2 py-2">{repoName(s, sw.repoId)}</td>
                    <td className="px-2 py-2 text-muted-foreground">{nodeName(s, sw.nodeId)}</td>
                    <td className="px-2 py-2">{w ? <WorkspaceStateLabel w={w} now={now} /> : <span className="text-faint">released {sw.releasedAt ? ago(sw.releasedAt, now) : ""}</span>}</td>
                    <td className="px-2 py-2 text-muted-foreground">{ago(sw.createdAt, now)}</td>
                    <td className="px-2 py-2">{w ? w.filesChanged : "—"}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      </Card>

      <Card title="Commands" sub="Every command the session ran, newest first: live ones, and those that ended in the last day" delay={60}>
        {rows.length === 0 ? (
          <Empty>Nothing run yet.</Empty>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-left text-[12px]">
              <thead className="eyebrow">
                <tr className="border-b border-border">
                  {["at", "state", "command", "repo", "node", "took", "speedup", "why"].map((h) => (
                    <th key={h} className="px-2 py-2 font-normal whitespace-nowrap">
                      {h}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody className="num font-mono text-[11px]">
                {rows.map((r) => (
                  <tr key={r.id} className="border-b border-border/60 align-top hover:bg-muted/60">
                    <td className="px-2 py-2 whitespace-nowrap text-faint" title={new Date(r.at).toISOString()}>
                      {hhmmss(r.at)}
                    </td>
                    <td className="px-2 py-2">
                      <StateBadge state={r.state} />
                    </td>
                    <td className="max-w-[420px] px-2 py-2">
                      <Link to="/commands/$id" params={{ id: r.id }} className="block truncate text-foreground hover:text-primary">
                        {r.argv}
                      </Link>
                      <div className="mt-0.5 flex gap-2">
                        <KindTag kind={r.kind} />
                        {r.url && (
                          <a href={r.url} className="text-[10px] text-primary hover:underline" target="_blank" rel="noreferrer">
                            {r.url.replace("http://", "")} ↗
                          </a>
                        )}
                      </div>
                    </td>
                    <td className="px-2 py-2 whitespace-nowrap text-muted-foreground">{r.repoId ? repoName(s, r.repoId) : "—"}</td>
                    <td className="px-2 py-2 whitespace-nowrap text-muted-foreground">{nodeName(s, r.nodeId)}</td>
                    <td className="px-2 py-2 whitespace-nowrap">{duration(r.seconds)}{r.live && <span className="text-faint"> so far</span>}</td>
                    <td className="px-2 py-2 whitespace-nowrap text-primary">{!r.live && r.coldSeconds !== null ? speedup(r.coldSeconds, r.seconds) : "—"}</td>
                    <td className="max-w-[260px] px-2 py-2 font-sans text-muted-foreground">
                      {r.live ? (r.reason ?? "") : r.failure ? FAILURE[r.failure] : r.state === "failed" && r.exitCode !== null ? `exit ${r.exitCode}` : ""}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    </div>
  );
}
