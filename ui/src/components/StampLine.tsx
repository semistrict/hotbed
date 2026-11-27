import type { AskedFrom, Snapshot, Stamp } from "../api";
import { ago, shortSha } from "../lib/format";
import { isUserActor } from "../lib/stamps";
import { SessionLink } from "./SessionLink";

/**
 * Who did something and when: "asked by brave-otter on asahimbp, 12m ago".
 *
 * `verb` leads it ("asked by …"; "" gives a bare "by …"; leaving it out
 * starts at the name, for callers that word it themselves). `actor` tags
 * the doer as a user or an agent and names the agent's model and subagent.
 * Given the snapshot `s`, a session links only while the cockpit still lists it.
 */
export function StampLine({
  st,
  now,
  verb,
  s,
  actor = false,
  hideModel = false,
}: {
  st: Stamp;
  now: number;
  verb?: string;
  s?: Pick<Snapshot, "sessions">;
  actor?: boolean;
  /** Leave out the model, when the caller already shows it. */
  hideModel?: boolean;
}) {
  const user = isUserActor(st);
  const linked = !s || s.sessions.some((x) => x.id === st.session);
  const title = [new Date(Number(st.at)).toLocaleString()];
  if (st.t3codeThreadId) title.push(`T3 thread: ${st.t3codeThreadId}`);
  if (st.subagent) title.push(`subagent: ${st.subagent}`);
  if (st.model) title.push(`model: ${st.model}`);
  return (
    <span title={title.join(" · ")}>
      {verb !== undefined && (verb ? `${verb} by ` : "by ")}
      {actor &&
        (user ? (
          <span className="rounded bg-ember-wash px-1 py-px font-mono text-[10px] font-medium text-primary">user</span>
        ) : (
          <span className="rounded border border-border px-1 py-px font-mono text-[10px] text-muted-foreground">agent</span>
        ))}
      {actor && " "}
      {user ? (
        <span className="text-foreground">{st.user || "you"}</span>
      ) : linked ? (
        <SessionLink id={st.session} />
      ) : (
        <span className="font-mono text-[11px]">{st.session}</span>
      )}
      {actor && !user && !hideModel && st.model && (
        <> <span className="rounded bg-muted px-1 py-px font-mono text-[10.5px] text-foreground">{st.model}</span></>
      )}
      {actor && !user && st.subagent && <span className="font-mono text-[10.5px] text-muted-foreground"> ({st.subagent})</span>}
      {st.host && <span className="text-faint"> on {st.host}</span>}, {ago(st.at, now)}
    </span>
  );
}

/** Where an agent was working when it acted: "from hotbed @ main 1a2b3c4d · 3 changed". */
export function FromLine({ f }: { f: AskedFrom }) {
  const repo = (f.remote || f.dir).replace(/\.git$/, "").split(/[/:]/).pop();
  return (
    <>
      from {repo}
      {f.branch && <> @ {f.branch}</>}
      {f.head && <span className="text-faint"> {shortSha(f.head)}</span>}
      {f.dirtyFiles > 0 && <span className="text-warning-ink"> · {f.dirtyFiles} changed</span>}
    </>
  );
}
