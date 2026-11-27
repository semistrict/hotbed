import {
  ArrowDown,
  ArrowUp,
  ArrowUpRight,
  Check,
  Copy,
  Database,
  Search,
} from "lucide-react";
import { useMemo, useState } from "react";
import { Badge as UiBadge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  formatDoTargetLabel,
  resolveCellDoLink,
  useDoLinkContext,
  type DoLinkResolutionContext,
  type DoTargetRef,
} from "../lib/doLinks";
import { bytes, duration } from "../lib/format";

type Tone = "default" | "ember" | "frost" | "good" | "warning" | "critical";

export type ColumnFormat =
  | "text"
  | "number"
  | "bytes"
  | "durationMs"
  | "timestamp"
  | "code"
  | "badge"
  | "json";

export interface SqlTableColumn {
  key: string;
  label: string;
  align?: "left" | "right";
  format?: ColumnFormat;
}

function toneBadgeClass(tone: Tone | undefined): string {
  switch (tone) {
    case "ember":
      return "border-primary/40 bg-ember-wash text-primary";
    case "frost":
      return "border-frost/40 bg-frost-wash text-frost";
    case "good":
      return "border-[color-mix(in_oklab,var(--good)_35%,transparent)] bg-[color-mix(in_oklab,var(--good)_12%,transparent)] text-good-ink";
    case "warning":
      return "border-[color-mix(in_oklab,var(--warning)_40%,transparent)] bg-[color-mix(in_oklab,var(--warning)_16%,transparent)] text-warning-ink";
    case "critical":
      return "border-critical/40 bg-[color-mix(in_oklab,var(--critical)_14%,transparent)] text-critical";
    default:
      return "border-border bg-secondary/70 text-foreground";
  }
}

function inferStatusTone(val: string): Tone {
  const lower = val.toLowerCase();
  if (
    lower.includes("fail") ||
    lower.includes("error") ||
    lower.includes("crit") ||
    lower.includes("dead") ||
    lower.includes("revoked")
  ) {
    return "critical";
  }
  if (
    lower.includes("warn") ||
    lower.includes("pend") ||
    lower.includes("queue") ||
    lower.includes("block") ||
    lower.includes("stale")
  ) {
    return "warning";
  }
  if (
    lower.includes("ok") ||
    lower.includes("good") ||
    lower.includes("pass") ||
    lower.includes("succ") ||
    lower.includes("online") ||
    lower.includes("active") ||
    lower.includes("approv") ||
    lower.includes("health") ||
    lower === "true"
  ) {
    return "good";
  }
  if (lower.includes("run") || lower.includes("live") || lower.includes("warm")) {
    return "ember";
  }
  if (lower.includes("park") || lower.includes("cold") || lower.includes("froz")) {
    return "frost";
  }
  return "default";
}

function formatTimestampMs(raw: unknown): { label: string; title: string } | null {
  const n = typeof raw === "number" ? raw : typeof raw === "string" && /^\d{10,16}$/.test(raw) ? Number(raw) : NaN;
  if (!Number.isFinite(n) || n <= 0) return null;
  const ms = n < 1e12 ? n * 1000 : n;
  const d = new Date(ms);
  if (Number.isNaN(d.getTime())) return null;
  const diffSec = Math.round((Date.now() - ms) / 1000);
  const abs = Math.abs(diffSec);
  let rel: string;
  if (abs < 60) rel = `${abs}s ${diffSec >= 0 ? "ago" : "from now"}`;
  else if (abs < 3600) rel = `${Math.round(abs / 60)}m ${diffSec >= 0 ? "ago" : "from now"}`;
  else if (abs < 86400) rel = `${Math.round(abs / 3600)}h ${diffSec >= 0 ? "ago" : "from now"}`;
  else rel = d.toLocaleDateString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
  return { label: rel, title: d.toISOString() };
}

function formatCell(value: unknown, format: ColumnFormat | undefined) {
  if (value === null || value === undefined) {
    return <span className="font-mono text-[11px] text-faint italic">NULL</span>;
  }

  if (format === "bytes") {
    const n = Number(value);
    if (Number.isFinite(n)) {
      return <span className="num font-mono text-[12px]">{bytes(n)}</span>;
    }
  }

  if (format === "durationMs") {
    const n = Number(value);
    if (Number.isFinite(n)) {
      const formatted = n < 1000 ? `${Math.round(n)}ms` : duration(n / 1000);
      return <span className="num font-mono text-[12px]">{formatted}</span>;
    }
  }

  if (format === "timestamp") {
    const ts = formatTimestampMs(value);
    if (ts) {
      return (
        <span className="num font-mono text-[11.5px] text-muted-foreground" title={ts.title}>
          {ts.label}
        </span>
      );
    }
  }

  if (format === "number") {
    const n = Number(value);
    if (Number.isFinite(n)) {
      return <span className="num font-mono text-[12px]">{n.toLocaleString()}</span>;
    }
  }

  if (format === "badge") {
    const str = String(value);
    return (
      <UiBadge
        variant="outline"
        className={`h-5 px-1.5 font-mono text-[10.5px] font-normal ${toneBadgeClass(inferStatusTone(str))}`}
      >
        {str}
      </UiBadge>
    );
  }

  if (format === "code") {
    return (
      <code className="rounded bg-secondary/80 px-1.5 py-0.5 font-mono text-[11.5px] text-foreground">
        {String(value)}
      </code>
    );
  }

  if (format === "json") {
    const str = typeof value === "string" ? value : JSON.stringify(value);
    return (
      <code className="block max-w-md truncate font-mono text-[11px] text-muted-foreground" title={str}>
        {str}
      </code>
    );
  }

  if (typeof value === "number") {
    return <span className="num font-mono text-[12px]">{value.toLocaleString()}</span>;
  }

  if (typeof value === "boolean") {
    return (
      <UiBadge variant="outline" className={`h-5 px-1.5 font-mono text-[10.5px] ${toneBadgeClass(value ? "good" : "default")}`}>
        {String(value)}
      </UiBadge>
    );
  }

  const str = typeof value === "string" ? value : JSON.stringify(value);
  return (
    <span className="block max-w-md truncate text-[12.5px]" title={str.length > 60 ? str : undefined}>
      {str}
    </span>
  );
}

function CopyInlineButton({ text, label = "Copy" }: { text: string; label?: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <Button
      type="button"
      size="sm"
      variant="ghost"
      className="h-6 gap-1 px-2 font-mono text-[10.5px] text-muted-foreground hover:text-foreground"
      onClick={() => {
        void navigator.clipboard?.writeText(text).then(() => {
          setCopied(true);
          window.setTimeout(() => setCopied(false), 1400);
        });
      }}
    >
      {copied ? <Check className="size-3 text-good-ink" /> : <Copy className="size-3" />}
      {copied ? "Copied" : label}
    </Button>
  );
}

function rowsToCsv(
  columns: ReadonlyArray<{ key: string; label: string }>,
  rows: ReadonlyArray<Record<string, unknown>>,
): string {
  const esc = (v: unknown) => {
    const s = v === null || v === undefined ? "" : typeof v === "string" ? v : JSON.stringify(v);
    return /[",\n\r]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
  };
  const header = columns.map((c) => esc(c.label)).join(",");
  const lines = rows.map((r) => columns.map((c) => esc(r[c.key])).join(","));
  return [header, ...lines].join("\n");
}

function DoLinkChip({
  value,
  target,
  onOpen,
}: {
  value: string;
  target: DoTargetRef;
  onOpen: (target: DoTargetRef) => void;
}) {
  const label = formatDoTargetLabel(target);
  return (
    <button
      type="button"
      onClick={() => onOpen(target)}
      title={`Open ${label} in SQL Explorer`}
      className="inline-flex max-w-md cursor-pointer items-center gap-1 rounded border border-primary/35 bg-ember-wash/70 px-1.5 py-0.5 font-mono text-[11.5px] text-primary transition-colors hover:border-primary/65 hover:bg-ember-wash"
    >
      <span className="truncate">{value}</span>
      <ArrowUpRight className="size-3 shrink-0 opacity-80" />
    </button>
  );
}

export function DataTableView({
  props,
  linkContext,
}: {
  props: {
    title?: string;
    caption?: string;
    columns: SqlTableColumn[];
    rows: Record<string, unknown>[];
    searchable?: boolean;
    maxHeight?: number;
  };
  linkContext?: Omit<DoLinkResolutionContext, "scope" | "allowedBindings">;
}) {
  const doLinks = useDoLinkContext();
  const [query, setQuery] = useState("");
  const [sortCol, setSortCol] = useState<string | null>(null);
  const [sortDir, setSortDir] = useState<"asc" | "desc">("asc");

  const resolutionCtx = useMemo<DoLinkResolutionContext>(
    () => ({
      ...linkContext,
      scope: doLinks?.scope,
      allowedBindings: doLinks?.allowedBindings,
    }),
    [doLinks?.allowedBindings, doLinks?.scope, linkContext],
  );

  const columns = props.columns ?? [];
  const rows = props.rows ?? [];
  const showSearch = props.searchable ?? rows.length > 6;

  const filteredAndSorted = useMemo(() => {
    const q = query.trim().toLowerCase();
    const filtered = q
      ? rows.filter((r) =>
          columns.some((c) => {
            const v = r[c.key];
            if (v === null || v === undefined) return false;
            const s = typeof v === "string" ? v : JSON.stringify(v);
            return s.toLowerCase().includes(q);
          }),
        )
      : rows;

    if (!sortCol) return filtered;
    const dir = sortDir === "asc" ? 1 : -1;
    return [...filtered].sort((a, b) => {
      const av = a[sortCol];
      const bv = b[sortCol];
      if (av === bv) return 0;
      if (av === null || av === undefined) return 1;
      if (bv === null || bv === undefined) return -1;
      if (typeof av === "number" && typeof bv === "number") return (av - bv) * dir;
      return String(av).localeCompare(String(bv), undefined, { numeric: true }) * dir;
    });
  }, [columns, query, rows, sortCol, sortDir]);

  function toggleSort(key: string) {
    if (sortCol !== key) {
      setSortCol(key);
      setSortDir("asc");
    } else if (sortDir === "asc") {
      setSortDir("desc");
    } else {
      setSortCol(null);
    }
  }

  return (
    <div className="overflow-hidden rounded-xl border border-border bg-card/70">
      {(props.title || showSearch || rows.length > 0) && (
        <div className="flex flex-wrap items-center justify-between gap-2 border-b border-border/80 bg-secondary/35 px-3 py-2">
          <div className="flex min-w-0 items-center gap-2">
            {props.title && <span className="truncate text-[12.5px] font-medium text-foreground">{props.title}</span>}
            <span className="num font-mono text-[10.5px] text-muted-foreground">
              {filteredAndSorted.length}
              {filteredAndSorted.length !== rows.length ? ` / ${rows.length}` : ""} rows
            </span>
          </div>
          <div className="flex items-center gap-1.5">
            {showSearch && (
              <div className="relative">
                <Search className="pointer-events-none absolute top-1/2 left-2 size-3 -translate-y-1/2 text-faint" />
                <Input
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  placeholder="Filter rows…"
                  className="h-6.5 w-36 bg-background/80 pl-6.5 font-mono text-[11px] sm:w-44"
                />
              </div>
            )}
            {rows.length > 0 && <CopyInlineButton text={rowsToCsv(columns, filteredAndSorted)} label="CSV" />}
            {rows.length > 0 && (
              <CopyInlineButton text={JSON.stringify(filteredAndSorted, null, 2)} label="JSON" />
            )}
          </div>
        </div>
      )}

      <div
        className="overflow-auto"
        style={props.maxHeight ? { maxHeight: `${props.maxHeight}px` } : { maxHeight: "520px" }}
      >
        <Table filterable={false}>
          <TableHeader className="sticky top-0 z-10 bg-card/95 backdrop-blur-sm">
            <TableRow className="border-border hover:bg-transparent">
              {columns.map((col) => {
                const active = sortCol === col.key;
                const alignRight =
                  col.align === "right" || col.format === "number" || col.format === "bytes" || col.format === "durationMs";
                return (
                  <TableHead
                    key={col.key}
                    className={`h-8 cursor-pointer select-none whitespace-nowrap px-3 font-mono text-[10.5px] tracking-wide text-muted-foreground uppercase transition-colors hover:text-foreground ${alignRight ? "text-right" : "text-left"}`}
                    onClick={() => toggleSort(col.key)}
                  >
                    <span className={`inline-flex items-center gap-1 ${alignRight ? "flex-row-reverse" : ""}`}>
                      <span>{col.label}</span>
                      {active &&
                        (sortDir === "asc" ? (
                          <ArrowUp className="size-3 text-primary" />
                        ) : (
                          <ArrowDown className="size-3 text-primary" />
                        ))}
                    </span>
                  </TableHead>
                );
              })}
            </TableRow>
          </TableHeader>
          <TableBody>
            {filteredAndSorted.length === 0 ? (
              <TableRow>
                <TableCell
                  colSpan={Math.max(1, columns.length)}
                  className="py-6 text-center font-mono text-[11.5px] text-muted-foreground"
                >
                  {rows.length === 0 ? "Query returned 0 rows." : "No rows match your filter."}
                </TableCell>
              </TableRow>
            ) : (
              filteredAndSorted.map((row, rIdx) => (
                <TableRow key={rIdx} className="border-border/60 hover:bg-secondary/35">
                  {columns.map((col) => {
                    const alignRight =
                      col.align === "right" ||
                      col.format === "number" ||
                      col.format === "bytes" ||
                      col.format === "durationMs";
                    const cellVal = row[col.key];
                    const doTarget = doLinks
                      ? resolveCellDoLink(cellVal, col.key, row, resolutionCtx)
                      : null;
                    return (
                      <TableCell
                        key={col.key}
                        className={`px-3 py-2 align-middle ${alignRight ? "text-right" : "text-left"}`}
                      >
                        {doTarget && doLinks ? (
                          <DoLinkChip
                            value={String(cellVal)}
                            target={doTarget}
                            onOpen={doLinks.onOpenDoTarget}
                          />
                        ) : (
                          formatCell(cellVal, col.format)
                        )}
                      </TableCell>
                    );
                  })}
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </div>

      {props.caption && (
        <div className="border-t border-border/70 px-3 py-1.5 font-mono text-[10.5px] text-muted-foreground">
          {props.caption}
        </div>
      )}
    </div>
  );
}

export function SqlBlockView({
  props,
}: {
  props: {
    sql: string;
    target?: string;
    rowCount?: number;
    rowsRead?: number;
    durationMs?: number;
    truncated?: boolean;
  };
}) {
  return (
    <div className="overflow-hidden rounded-xl border border-border bg-secondary/45">
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-border/70 px-3 py-1.5">
        <div className="flex flex-wrap items-center gap-2 font-mono text-[10.5px] text-muted-foreground">
          <Database className="size-3.5 text-primary" />
          <span className="font-medium text-foreground">{props.target || "SQLite Query"}</span>
          {props.rowCount !== undefined && (
            <span className="num">
              {props.rowCount} rows{props.truncated ? " (capped)" : ""}
            </span>
          )}
          {props.rowsRead !== undefined && <span className="num">· {props.rowsRead} read</span>}
          {props.durationMs !== undefined && <span className="num">· {props.durationMs}ms</span>}
        </div>
        <CopyInlineButton text={props.sql} label="Copy SQL" />
      </div>
      <pre className="overflow-x-auto px-3 py-2.5 font-mono text-[11.5px] leading-relaxed text-foreground">
        <code>{props.sql}</code>
      </pre>
    </div>
  );
}
