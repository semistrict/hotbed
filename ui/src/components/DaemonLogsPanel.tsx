import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Fragment, useState } from "react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api, DAEMON_LOG_LEVEL_FROM_SLUG, DaemonLogLevel, type DaemonLogEntry, type DaemonLogLevelSlug, type DaemonLogSource } from "../api";
import { bytes, hhmmss } from "../lib/format";
import { Card, Empty } from "./Panel";

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
        <Badge variant="outline" className="num font-mono text-[10.5px] font-normal text-muted-foreground" title="Log rotation usage on disk">
          {bytes(activeSourceInfo.diskBytes)} · {activeSourceInfo.files}/{activeSourceInfo.maxFiles || 5} files
        </Badge>
      )}
      <Button
        type="button"
        variant="outline"
        size="xs"
        onClick={() => setLive((v) => !v)}
        className={`gap-1.5 font-mono text-[10.5px] ${
          live ? "border-primary/40 bg-ember-wash text-primary hover:bg-ember-wash" : "text-muted-foreground"
        }`}
      >
        {live && <span className="live-dot inline-block size-1.5 rounded-full bg-primary" />}
        {live ? "Live 5s" : "Paused"}
      </Button>
      <Button
        type="button"
        variant="outline"
        size="xs"
        onClick={() => logsQuery.refetch()}
        className="font-mono text-[10.5px] text-muted-foreground hover:text-foreground"
      >
        Refresh
      </Button>
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
            <Input
              type="search"
              value={queryText}
              onChange={(e) => setQueryText(e.target.value)}
              placeholder="Filter messages, attrs, source file..."
              aria-label="Filter daemon logs"
              className="min-w-[220px] flex-1 bg-background/70 font-mono text-[11.5px] placeholder:text-faint"
            />
            {!lockedSource && sources.length > 0 && (
              <Tabs value={source || "__all__"} onValueChange={(v) => setSource(v === "__all__" ? "" : String(v))}>
                <TabsList className="h-8 border border-border bg-background/60 font-mono text-[11px]">
                  <TabsTrigger value="__all__" className="px-2 font-mono text-[11px]">
                    All
                  </TabsTrigger>
                  {sources.map((src) => (
                    <TabsTrigger key={`${src.role}-${src.source}`} value={src.source} className="px-2 font-mono text-[11px]">
                      {src.source}
                    </TabsTrigger>
                  ))}
                </TabsList>
              </Tabs>
            )}
            <Tabs value={level || "__all__"} onValueChange={(v) => setLevel((v === "__all__" ? "" : v) as DaemonLogLevelSlug | "")}>
              <TabsList className="h-8 border border-border bg-background/60 font-mono text-[11px]">
                <TabsTrigger value="__all__" className="px-2 font-mono text-[11px]">
                  Warn + Error
                </TabsTrigger>
                <TabsTrigger value="error" className="px-2 font-mono text-[11px]">
                  Errors
                </TabsTrigger>
                <TabsTrigger value="warn" className="px-2 font-mono text-[11px]">
                  Warns
                </TabsTrigger>
              </TabsList>
            </Tabs>
            <Tabs value={since || "__all__"} onValueChange={(v) => setSince((v === "__all__" ? "" : v) as SinceKey)}>
              <TabsList className="h-8 border border-border bg-background/60 font-mono text-[11px]">
                {(
                  [
                    ["15m", "15m"],
                    ["1h", "1h"],
                    ["24h", "24h"],
                    ["7d", "7d"],
                    ["__all__", "All"],
                  ] as const
                ).map(([val, label]) => (
                  <TabsTrigger key={val} value={val} className="px-2 font-mono text-[11px]">
                    {label}
                  </TabsTrigger>
                ))}
              </TabsList>
            </Tabs>
          </div>

          {unreachable.length > 0 && (
            <Alert className="border-warning/40 bg-[color-mix(in_oklab,var(--warning)_10%,transparent)] px-3 py-2 font-mono text-[11px]">
              <AlertDescription className="text-[11px] text-foreground">
                {unreachable.map((u) => (
                  <div key={u.source}>
                    <span className="font-semibold">{u.source}</span> ({u.role}): unreachable{u.error ? ` — ${u.error}` : ""}
                  </div>
                ))}
              </AlertDescription>
            </Alert>
          )}

          {logsQuery.isLoading ? (
            <div className="py-10 text-center font-mono text-[11.5px] text-muted-foreground">Loading daemon logs…</div>
          ) : logsQuery.error ? (
            <Alert variant="destructive" className="border-critical/35 bg-[color-mix(in_oklab,var(--critical)_10%,transparent)] px-3 py-2 font-mono text-[11.5px]">
              <AlertDescription className="text-[11.5px] text-critical">{String(logsQuery.error)}</AlertDescription>
            </Alert>
          ) : entries.length === 0 ? (
            <Empty>
              No WARN or ERROR slog entries match this filter{since ? ` in the last ${since}` : ""}.
            </Empty>
          ) : (
            <>
              <div className={`${compact ? "max-h-[380px]" : "max-h-[680px]"} overflow-auto rounded-lg border border-border`}>
                <Table className="font-mono text-[11.5px]">
                  <TableHeader className="sticky top-0 z-10 bg-card text-[10px] uppercase tracking-wider text-muted-foreground">
                    <TableRow className="border-border hover:bg-transparent">
                      <TableHead className="h-auto w-[140px] px-3 py-2 font-normal text-muted-foreground">Time</TableHead>
                      <TableHead className="h-auto w-[72px] px-2 py-2 font-normal text-muted-foreground">Level</TableHead>
                      {!lockedSource && <TableHead className="h-auto w-[128px] px-2 py-2 font-normal text-muted-foreground">Source</TableHead>}
                      <TableHead className="h-auto px-2 py-2 font-normal text-muted-foreground">Message & attributes</TableHead>
                      <TableHead className="hidden h-auto w-[220px] px-3 py-2 text-right font-normal text-muted-foreground xl:table-cell">Location</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody className="divide-y divide-border/70">
                    {entries.map((entry, idx) => {
                      const rowKey = `${entry.at}-${entry.source}-${idx}`;
                      const isOpen = expanded === rowKey;
                      const attrs = parseAttrs(entry.attrsJson);
                      return (
                        <Fragment key={rowKey}>
                          <TableRow
                            onClick={() => setExpanded(isOpen ? null : rowKey)}
                            className={`cursor-pointer align-top hover:bg-muted/50 ${isOpen ? "bg-muted/40" : ""}`}
                          >
                            <TableCell className="num px-3 py-2 text-faint" title={new Date(Number(entry.at)).toISOString()}>
                              {formatEntryTime(entry.at)}
                            </TableCell>
                            <TableCell className="px-2 py-2">
                              <LevelBadge level={entry.level} />
                            </TableCell>
                            {!lockedSource && (
                              <TableCell className="px-2 py-2">
                                <SourceBadge entry={entry} />
                              </TableCell>
                            )}
                            <TableCell className="px-2 py-2 whitespace-normal">
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
                            </TableCell>
                            <TableCell className="hidden px-3 py-2 text-right text-[10.5px] text-faint xl:table-cell" title={entry.sourceLoc}>
                              {entry.sourceLoc || "—"}
                            </TableCell>
                          </TableRow>
                          {isOpen && (
                            <TableRow className="bg-background/60 hover:bg-background/60">
                              <TableCell colSpan={lockedSource ? 4 : 5} className="px-4 py-3 text-[11px] whitespace-normal">
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
                              </TableCell>
                            </TableRow>
                          )}
                        </Fragment>
                      );
                    })}
                  </TableBody>
                </Table>
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
        <Badge variant="secondary" className="h-auto rounded px-1.5 py-0.5 font-mono text-[9.5px] uppercase tracking-wider text-muted-foreground">
          {src.role}
        </Badge>
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

function LevelBadge({ level }: { level: DaemonLogLevel }) {
  if (level === DaemonLogLevel.ERROR) {
    return (
      <Badge variant="destructive" className="h-auto rounded bg-[color-mix(in_oklab,var(--critical)_16%,transparent)] px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wider text-critical">
        ERROR
      </Badge>
    );
  }
  return (
    <Badge variant="secondary" className="h-auto rounded bg-[color-mix(in_oklab,var(--warning)_20%,transparent)] px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wider text-warning">
      WARN
    </Badge>
  );
}

function SourceBadge({ entry }: { entry: DaemonLogEntry }) {
  if (entry.role === "controller") {
    return (
      <Badge variant="secondary" className="h-auto rounded bg-ember-wash px-1.5 py-0.5 text-[10.5px] font-normal text-primary" title={`controller on ${entry.host}`}>
        controller
      </Badge>
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
