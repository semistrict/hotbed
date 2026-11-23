import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Fragment, useState } from "react";
import { api, DAEMON_LOG_LEVEL_FROM_SLUG, DaemonLogLevel, type DaemonLogEntry, type DaemonLogLevelSlug, type DaemonLogSource } from "../api";
import { bytes, hhmmss } from "../lib/format";
import { Card, Empty } from "./ui/Card";

type SinceKey = "15m" | "1h" | "24h" | "7d" | "";

export interface DaemonLogsPanelProps {
  /** Lock the panel to one daemon ("controller" or worker hostname). When omitted, shows all daemons with a source picker. */
  lockedSource?: string;
  /** Initial / controlled source filter when not locked. */
  source?: string;
  onSourceChange?: (source: string) => void;
  /** Initial / controlled level filter. */
  level?: DaemonLogLevelSlug | "";
  onLevelChange?: (level: DaemonLogLevelSlug | "") => void;
  /** Initial / controlled since window. */
  since?: SinceKey;
  onSinceChange?: (since: SinceKey) => void;
  /** Compact mode for embedding inside NodePage. */
  compact?: boolean;
  /** Optional header action on the right of the card. */
  headerRight?: React.ReactNode;
}

export function DaemonLogsPanel({
  lockedSource,
  source: controlledSource,
  onSourceChange,
  level: controlledLevel,
  onLevelChange,
  since: controlledSince,
  onSinceChange,
  compact = false,
  headerRight,
}: DaemonLogsPanelProps) {
  const [localSource, setLocalSource] = useState(controlledSource ?? "");
  const [localLevel, setLocalLevel] = useState<DaemonLogLevelSlug | "">(controlledLevel ?? "");
  const [localSince, setLocalSince] = useState<SinceKey>(controlledSince ?? "24h");
  const [queryText, setQueryText] = useState("");
  const [live, setLive] = useState(true);
  const [expanded, setExpanded] = useState<string | null>(null);

  const source = lockedSource ?? (controlledSource !== undefined ? controlledSource : localSource);
  const level = controlledLevel !== undefined ? controlledLevel : localLevel;
  const since = controlledSince !== undefined ? controlledSince : localSince;

  const setSource = (next: string) => {
    setLocalSource(next);
    onSourceChange?.(next);
  };
  const setLevel = (next: DaemonLogLevelSlug | "") => {
    setLocalLevel(next);
    onLevelChange?.(next);
  };
  const setSince = (next: SinceKey) => {
    setLocalSince(next);
    onSinceChange?.(next);
  };

  const logsQuery = useQuery({
    queryKey: ["daemon-logs", source, level, since, queryText, compact ? 100 : 300],
    queryFn: () =>
      api.daemonLogs({
        source: source || undefined,
        level: level ? DAEMON_LOG_LEVEL_FROM_SLUG[level] : undefined,
        since: since || undefined,
        query: queryText.trim() || undefined,
        limit: compact ? 100 : 300,
      }),
    refetchInterval: live ? 5000 : false,
  });

  const data = logsQuery.data;
  const sources = data?.sources ?? [];
  const entries = data?.entries ?? [];
  const unreachable = sources.filter((s) => !s.reachable);
  const activeSourceInfo = lockedSource ? sources[0] : sources.find((s) => s.source === source);

  const controlsRight = (
    <div className="flex flex-wrap items-center gap-2 font-mono text-[10.5px]">
      {activeSourceInfo && (
        <span className="num rounded-full border border-border px-2.5 py-0.5 text-muted-foreground" title="Log rotation usage on disk">
          {bytes(activeSourceInfo.diskBytes)} · {activeSourceInfo.files}/{activeSourceInfo.maxFiles || 5} files
        </span>
      )}
      <button
        type="button"
        onClick={() => setLive((v) => !v)}
        className={`inline-flex items-center gap-1.5 rounded-md border px-2 py-0.5 transition-colors ${
          live ? "border-primary/40 bg-ember-wash text-primary" : "border-border text-muted-foreground hover:text-foreground"
        }`}
      >
        {live && <span className="live-dot inline-block size-1.5 rounded-full bg-primary" />}
        {live ? "Live 5s" : "Paused"}
      </button>
      <button
        type="button"
        onClick={() => logsQuery.refetch()}
        className="rounded-md border border-border px-2 py-0.5 text-muted-foreground hover:border-border-strong hover:text-foreground"
      >
        Refresh
      </button>
      {headerRight}
    </div>
  );

  return (
    <div className="space-y-4">
      {!lockedSource && sources.length > 0 && (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
          {sources.map((src) => (
            <SourceCard
              key={`${src.role}-${src.source}`}
              src={src}
              selected={source === src.source}
              onClick={() => setSource(source === src.source ? "" : src.source)}
            />
          ))}
        </div>
      )}

      <Card
        title={lockedSource ? "Daemon logs (WARN / ERROR)" : source ? `${source} logs` : "All daemon logs"}
        sub={
          lockedSource
            ? "Rotating WARN and ERROR slog records on this worker"
            : "WARN and ERROR slog records from the controller and every worker, newest first · rotated by size and age"
        }
        right={controlsRight}
      >
        <div className="space-y-3">
          {/* Filters */}
          <div className="flex flex-wrap items-center gap-2">
            <input
              type="search"
              value={queryText}
              onChange={(e) => setQueryText(e.target.value)}
              placeholder="Filter messages, attrs, source file..."
              aria-label="Filter daemon logs"
              className="min-w-[220px] flex-1 rounded-lg border border-border bg-background/70 px-3 py-1.5 font-mono text-[11.5px] text-foreground placeholder:text-faint focus:border-primary focus:outline-none"
            />
            {!lockedSource && sources.length > 0 && (
              <div className="flex flex-wrap items-center rounded-lg border border-border bg-background/60 p-0.5 font-mono text-[11px]">
                <SegButton active={source === ""} onClick={() => setSource("")}>
                  All
                </SegButton>
                {sources.map((src) => (
                  <SegButton key={`${src.role}-${src.source}`} active={source === src.source} onClick={() => setSource(src.source)}>
                    {src.source}
                  </SegButton>
                ))}
              </div>
            )}
            <div className="flex items-center rounded-lg border border-border bg-background/60 p-0.5 font-mono text-[11px]">
              {(
                [
                  ["", "Warn + Error"],
                  ["error", "Errors"],
                  ["warn", "Warns"],
                ] as const
              ).map(([val, label]) => (
                <SegButton key={val || "all"} active={level === val} onClick={() => setLevel(val)}>
                  {label}
                </SegButton>
              ))}
            </div>
            <div className="flex items-center rounded-lg border border-border bg-background/60 p-0.5 font-mono text-[11px]">
              {(
                [
                  ["15m", "15m"],
                  ["1h", "1h"],
                  ["24h", "24h"],
                  ["7d", "7d"],
                  ["", "All"],
                ] as const
              ).map(([val, label]) => (
                <SegButton key={val || "all"} active={since === val} onClick={() => setSince(val)}>
                  {label}
                </SegButton>
              ))}
            </div>
          </div>

          {unreachable.length > 0 && (
            <div className="rounded-lg border border-warning/40 bg-[color-mix(in_oklab,var(--warning)_10%,transparent)] px-3 py-2 font-mono text-[11px] text-foreground">
              {unreachable.map((u) => (
                <div key={u.source}>
                  <span className="font-semibold">{u.source}</span> ({u.role}): unreachable{u.error ? ` — ${u.error}` : ""}
                </div>
              ))}
            </div>
          )}

          {logsQuery.isLoading ? (
            <div className="py-10 text-center font-mono text-[11.5px] text-muted-foreground">Loading daemon logs…</div>
          ) : logsQuery.error ? (
            <div className="rounded-lg border border-critical/35 bg-[color-mix(in_oklab,var(--critical)_10%,transparent)] px-3 py-2 font-mono text-[11.5px] text-critical">
              {String(logsQuery.error)}
            </div>
          ) : entries.length === 0 ? (
            <Empty>
              No WARN or ERROR slog entries match this filter{since ? ` in the last ${since}` : ""}.
            </Empty>
          ) : (
            <>
              <div className={`${compact ? "max-h-[380px]" : "max-h-[680px]"} overflow-auto rounded-lg border border-border`}>
                <table className="w-full border-collapse text-left font-mono text-[11.5px]">
                  <thead className="sticky top-0 z-10 bg-card text-[10px] uppercase tracking-wider text-muted-foreground">
                    <tr className="border-b border-border">
                      <th className="w-[140px] px-3 py-2 font-normal">Time</th>
                      <th className="w-[72px] px-2 py-2 font-normal">Level</th>
                      {!lockedSource && <th className="w-[128px] px-2 py-2 font-normal">Source</th>}
                      <th className="px-2 py-2 font-normal">Message & attributes</th>
                      <th className="hidden w-[220px] px-3 py-2 text-right font-normal xl:table-cell">Location</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-border/70">
                    {entries.map((entry, idx) => {
                      const rowKey = `${entry.at}-${entry.source}-${idx}`;
                      const isOpen = expanded === rowKey;
                      const attrs = parseAttrs(entry.attrsJson);
                      return (
                        <Fragment key={rowKey}>
                          <tr
                            onClick={() => setExpanded(isOpen ? null : rowKey)}
                            className={`cursor-pointer align-top transition-colors hover:bg-muted/50 ${isOpen ? "bg-muted/40" : ""}`}
                          >
                            <td className="num px-3 py-2 whitespace-nowrap text-faint" title={new Date(Number(entry.at)).toISOString()}>
                              {formatEntryTime(entry.at)}
                            </td>
                            <td className="px-2 py-2 whitespace-nowrap">
                              <LevelBadge level={entry.level} />
                            </td>
                            {!lockedSource && (
                              <td className="px-2 py-2 whitespace-nowrap">
                                <SourceBadge entry={entry} />
                              </td>
                            )}
                            <td className="px-2 py-2">
                              <div className="break-words text-foreground">{entry.msg}</div>
                              {attrs.length > 0 && (
                                <div className="mt-1 flex flex-wrap gap-1.5">
                                  {attrs.slice(0, isOpen ? attrs.length : 5).map(([k, v]) => (
                                    <span
                                      key={k}
                                      className={`inline-flex max-w-[420px] truncate rounded px-1.5 py-0.5 text-[10.5px] ${
                                        k === "err" || k === "error"
                                          ? "bg-[color-mix(in_oklab,var(--critical)_14%,transparent)] text-critical"
                                          : "bg-muted text-muted-foreground"
                                      }`}
                                      title={`${k}=${v}`}
                                    >
                                      <span className="text-faint">{k}=</span>
                                      <span className="truncate">{v}</span>
                                    </span>
                                  ))}
                                  {!isOpen && attrs.length > 5 && <span className="text-[10px] text-faint">+{attrs.length - 5} more</span>}
                                </div>
                              )}
                            </td>
                            <td className="hidden px-3 py-2 text-right text-[10.5px] whitespace-nowrap text-faint xl:table-cell" title={entry.sourceLoc}>
                              {entry.sourceLoc || "—"}
                            </td>
                          </tr>
                          {isOpen && (
                            <tr className="bg-background/60">
                              <td colSpan={lockedSource ? 4 : 5} className="px-4 py-3 text-[11px]">
                                <div className="flex flex-wrap gap-x-6 gap-y-1 text-muted-foreground">
                                  <span>
                                    <span className="text-faint">timestamp:</span> {new Date(Number(entry.at)).toISOString()}
                                  </span>
                                  <span>
                                    <span className="text-faint">daemon:</span> {entry.source} ({entry.role} on {entry.host || entry.source})
                                  </span>
                                  {entry.sourceLoc && (
                                    <span>
                                      <span className="text-faint">location:</span> {entry.sourceLoc}
                                    </span>
                                  )}
                                </div>
                                {entry.attrsJson && (
                                  <pre className="mt-2 max-h-60 overflow-auto rounded-md border border-border bg-card p-2.5 text-[11px] leading-relaxed text-foreground">
                                    {prettyJson(entry.attrsJson)}
                                  </pre>
                                )}
                              </td>
                            </tr>
                          )}
                        </Fragment>
                      );
                    })}
                  </tbody>
                </table>
              </div>
              <div className="flex flex-wrap items-center justify-between font-mono text-[10.5px] text-faint">
                <span>
                  {entries.length} {entries.length === 1 ? "entry" : "entries"} shown{data?.truncated ? " (truncated — narrow filter or time window to see more)" : ""}
                </span>
                <span>Click any row to inspect full structured JSON</span>
              </div>
            </>
          )}
        </div>
      </Card>
    </div>
  );
}

function SourceCard({ src, selected, onClick }: { src: DaemonLogSource; selected: boolean; onClick: () => void }) {
  const cap = src.maxFileBytes > 0n && src.maxFiles > 0 ? `${bytes(src.maxFileBytes)} × ${src.maxFiles} · ${src.maxAgeDays || 14}d` : "";
  return (
    <button
      type="button"
      onClick={onClick}
      className={`rounded-2xl border p-4 text-left transition-colors ${
        selected
          ? "border-primary bg-ember-wash/40"
          : "border-border bg-card/80 hover:border-border-strong"
      }`}
    >
      <div className="flex items-center gap-2">
        <span className={`inline-block size-2 rounded-full ${src.reachable ? "bg-good" : "bg-critical"}`} />
        <span className="font-mono text-[13px] font-semibold text-foreground">{src.source}</span>
        <span className="rounded bg-muted px-1.5 py-0.5 font-mono text-[9.5px] uppercase tracking-wider text-muted-foreground">
          {src.role}
        </span>
      </div>
      <div className="mt-2.5 flex items-baseline gap-3 font-mono text-[12px]">
        <span className={src.errors24h > 0 ? "text-critical" : "text-muted-foreground"}>
          <span className="num font-semibold">{src.errors24h}</span> err
        </span>
        <span className={src.warns24h > 0 ? "text-warning" : "text-muted-foreground"}>
          <span className="num font-semibold">{src.warns24h}</span> warn
        </span>
        <span className="ml-auto text-[10px] text-faint">24h</span>
      </div>
      <div className="mt-2 flex items-center justify-between font-mono text-[10px] text-faint">
        <span>
          {bytes(src.diskBytes)} · {src.files}/{src.maxFiles || 5} files
        </span>
        {cap && <span title="Rotation policy">{cap}</span>}
      </div>
      {!src.reachable && src.error && (
        <div className="mt-1.5 truncate font-mono text-[10px] text-critical" title={src.error}>
          {src.error}
        </div>
      )}
    </button>
  );
}

function SegButton({ active, onClick, children }: { active: boolean; onClick: () => void; children: React.ReactNode }) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`rounded-md px-2 py-1 transition-colors ${active ? "bg-muted text-foreground" : "text-muted-foreground hover:text-foreground"}`}
    >
      {children}
    </button>
  );
}

function LevelBadge({ level }: { level: DaemonLogLevel }) {
  if (level === DaemonLogLevel.ERROR) {
    return (
      <span className="inline-block rounded bg-[color-mix(in_oklab,var(--critical)_16%,transparent)] px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wider text-critical">
        ERROR
      </span>
    );
  }
  return (
    <span className="inline-block rounded bg-[color-mix(in_oklab,var(--warning)_20%,transparent)] px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wider text-warning">
      WARN
    </span>
  );
}

function SourceBadge({ entry }: { entry: DaemonLogEntry }) {
  if (entry.role === "controller") {
    return (
      <span className="inline-block rounded bg-ember-wash px-1.5 py-0.5 text-[10.5px] text-primary" title={`controller on ${entry.host}`}>
        controller
      </span>
    );
  }
  return (
    <Link
      to="/nodes/$hostname"
      params={{ hostname: entry.source }}
      onClick={(e) => e.stopPropagation()}
      className="inline-block rounded bg-muted px-1.5 py-0.5 text-[10.5px] text-foreground hover:text-primary"
    >
      {entry.source}
    </Link>
  );
}

function formatEntryTime(ms: number | bigint): string {
  const n = Number(ms);
  if (!n) return "—";
  const d = new Date(n);
  const now = new Date();
  const sameDay = d.getFullYear() === now.getFullYear() && d.getMonth() === now.getMonth() && d.getDate() === now.getDate();
  if (sameDay) return hhmmss(n);
  const mo = String(d.getMonth() + 1).padStart(2, "0");
  const da = String(d.getDate()).padStart(2, "0");
  return `${mo}-${da} ${hhmmss(n)}`;
}

function parseAttrs(raw: string): [string, string][] {
  if (!raw) return [];
  try {
    const obj = JSON.parse(raw) as unknown;
    if (!obj || typeof obj !== "object" || Array.isArray(obj)) return [];
    return Object.entries(obj as Record<string, unknown>).map(([k, v]) => [k, typeof v === "string" ? v : JSON.stringify(v)]);
  } catch {
    return [];
  }
}

function prettyJson(raw: string): string {
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    return raw;
  }
}
