import { defineRegistry } from "@json-render/react";
import {
  AlertCircle,
  AlertTriangle,
  ArrowDown,
  ArrowUp,
  ArrowUpRight,
  Check,
  CheckCircle2,
  ChevronDown,
  ChevronRight,
  Copy,
  Database,
  Info,
  Search,
} from "lucide-react";
import { useMemo, useState } from "react";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
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
import { adminAgentCatalog } from "../lib/adminAgentCatalog";
import {
  formatDoTargetLabel,
  resolveCellDoLink,
  useDoLinkContext,
  type DoLinkResolutionContext,
  type DoTargetRef,
} from "../lib/doLinks";
import { bytes, duration } from "../lib/format";

type Tone = "default" | "ember" | "frost" | "good" | "warning" | "critical";

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

function toneMetricClass(tone: Tone | undefined): string {
  switch (tone) {
    case "ember":
      return "text-primary";
    case "frost":
      return "text-frost";
    case "good":
      return "text-good-ink";
    case "warning":
      return "text-warning-ink";
    case "critical":
      return "text-critical";
    default:
      return "text-foreground";
  }
}

function toneBarClass(tone: Tone | undefined): string {
  switch (tone) {
    case "frost":
      return "bg-frost";
    case "good":
      return "bg-good";
    case "warning":
      return "bg-warning";
    case "critical":
      return "bg-critical";
    case "ember":
    default:
      return "bg-primary";
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
  // Treat 10-digit values as seconds, 13-digit values as milliseconds.
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

function formatCell(
  value: unknown,
  format: "text" | "number" | "bytes" | "durationMs" | "timestamp" | "code" | "badge" | "json" | undefined,
) {
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

function DataTableView({
  props,
  linkContext,
}: {
  props: {
    title?: string;
    caption?: string;
    columns: Array<{
      key: string;
      label: string;
      align?: "left" | "right";
      format?: "text" | "number" | "bytes" | "durationMs" | "timestamp" | "code" | "badge" | "json";
    }>;
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
        style={props.maxHeight ? { maxHeight: `${props.maxHeight}px` } : { maxHeight: "420px" }}
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

function SqlBlockView({
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

function CodeBlockView({
  props,
}: {
  props: {
    title?: string;
    language?: string;
    code: string;
    collapsible?: boolean;
  };
}) {
  const [open, setOpen] = useState(!props.collapsible);
  return (
    <div className="overflow-hidden rounded-xl border border-border bg-secondary/45">
      <div className="flex items-center justify-between gap-2 border-b border-border/70 px-3 py-1.5">
        <button
          type="button"
          className="flex items-center gap-1.5 font-mono text-[11px] font-medium text-foreground"
          onClick={() => props.collapsible && setOpen((v) => !v)}
        >
          {props.collapsible &&
            (open ? <ChevronDown className="size-3.5 text-muted-foreground" /> : <ChevronRight className="size-3.5 text-muted-foreground" />)}
          <span>{props.title || props.language || "Output"}</span>
        </button>
        <CopyInlineButton text={props.code} />
      </div>
      {open && (
        <pre className="max-h-72 overflow-auto px-3 py-2.5 font-mono text-[11.5px] leading-relaxed text-foreground">
          <code>{props.code}</code>
        </pre>
      )}
    </div>
  );
}

function KeyValueListView({
  items,
}: {
  items: Array<{
    key: string;
    value: string | number | boolean | null;
    mono?: boolean;
    badge?: boolean;
    tone?: Tone;
  }>;
}) {
  const doLinks = useDoLinkContext();
  const resolutionCtx = useMemo<DoLinkResolutionContext>(
    () => ({
      scope: doLinks?.scope,
      allowedBindings: doLinks?.allowedBindings,
    }),
    [doLinks?.allowedBindings, doLinks?.scope],
  );

  return (
    <dl className="grid grid-cols-1 gap-x-6 gap-y-2 rounded-xl border border-border bg-card/70 px-3.5 py-3 sm:grid-cols-2">
      {items.map((item, idx) => {
        const doTarget = doLinks
          ? resolveCellDoLink(item.value, item.key, {}, resolutionCtx)
          : null;
        return (
          <div
            key={`${item.key}-${idx}`}
            className="flex min-w-0 items-baseline justify-between gap-2 border-b border-border/50 pb-1.5 last:border-b-0"
          >
            <dt className="shrink-0 font-mono text-[11px] text-muted-foreground">{item.key}</dt>
            <dd className="min-w-0 truncate text-right text-[12px] text-foreground">
              {item.value === null ? (
                <span className="font-mono text-[11px] text-faint italic">NULL</span>
              ) : doTarget && doLinks ? (
                <DoLinkChip
                  value={String(item.value)}
                  target={doTarget}
                  onOpen={doLinks.onOpenDoTarget}
                />
              ) : item.badge ? (
                <UiBadge variant="outline" className={`font-mono text-[10.5px] ${toneBadgeClass(item.tone)}`}>
                  {String(item.value)}
                </UiBadge>
              ) : (
                <span className={item.mono ? "font-mono text-[11.5px]" : ""}>{String(item.value)}</span>
              )}
            </dd>
          </div>
        );
      })}
    </dl>
  );
}

export const { registry: adminAgentRegistry } = defineRegistry(adminAgentCatalog, {
  components: {
    Stack: ({ props, children }) => {
      const dir = props.direction === "horizontal" ? "flex-row items-center" : "flex-col";
      const gap = props.gap === "sm" ? "gap-2" : props.gap === "lg" ? "gap-4" : "gap-3";
      const wrap = props.wrap ? "flex-wrap" : "";
      return <div className={`flex min-w-0 ${dir} ${gap} ${wrap}`}>{children}</div>;
    },

    Grid: ({ props, children }) => {
      const cols =
        props.columns === 1
          ? "grid-cols-1"
          : props.columns === 2
            ? "grid-cols-1 sm:grid-cols-2"
            : props.columns === 3
              ? "grid-cols-1 sm:grid-cols-2 lg:grid-cols-3"
              : "grid-cols-2 sm:grid-cols-4";
      const gap = props.gap === "sm" ? "gap-2.5" : "gap-3.5";
      return <div className={`grid min-w-0 ${cols} ${gap}`}>{children}</div>;
    },

    Section: ({ props, children }) => (
      <section className="min-w-0 rounded-2xl border border-border bg-card/80 p-4">
        <header className="mb-3 flex flex-wrap items-start justify-between gap-2">
          <div className="min-w-0">
            <h4 className="text-[13.5px] font-semibold text-foreground">{props.title}</h4>
            {props.subtitle && (
              <p className="mt-0.5 font-mono text-[11px] text-muted-foreground">{props.subtitle}</p>
            )}
          </div>
          {props.badge && (
            <UiBadge variant="outline" className={`font-mono text-[10.5px] ${toneBadgeClass(props.tone)}`}>
              {props.badge}
            </UiBadge>
          )}
        </header>
        <div className="space-y-3">{children}</div>
      </section>
    ),

    Metric: ({ props }) => (
      <div className="min-w-0 rounded-xl border border-border bg-card/75 px-3.5 py-2.5">
        <div className="eyebrow truncate text-[10px]">{props.label}</div>
        <div className="mt-1 flex items-baseline gap-1">
          <span className={`num font-mono text-[22px] leading-none font-semibold ${toneMetricClass(props.tone)}`}>
            {typeof props.value === "number" ? props.value.toLocaleString() : props.value}
          </span>
          {props.unit && <span className="font-mono text-[11px] text-muted-foreground">{props.unit}</span>}
        </div>
        {props.detail && (
          <div className="mt-1 truncate font-mono text-[10.5px] text-muted-foreground">{props.detail}</div>
        )}
      </div>
    ),

    DataTable: ({ props }) => <DataTableView props={props} />,

    BarList: ({ props }) => {
      const items = props.items ?? [];
      const computedMax = props.max ?? Math.max(1, ...items.map((i) => Math.max(0, i.value)));
      return (
        <div className="rounded-xl border border-border bg-card/70 p-3.5">
          {props.title && (
            <div className="mb-2.5 text-[12px] font-medium text-foreground">{props.title}</div>
          )}
          <div className="space-y-2">
            {items.map((item, idx) => {
              const pct = Math.max(0, Math.min(100, (item.value / computedMax) * 100));
              return (
                <div key={`${item.label}-${idx}`} className="space-y-1">
                  <div className="flex items-baseline justify-between gap-2 text-[12px]">
                    <span className="truncate font-mono text-[11.5px] text-foreground">{item.label}</span>
                    <span className="num shrink-0 font-mono text-[11.5px] text-muted-foreground">
                      {item.formattedValue ?? item.value.toLocaleString()}
                    </span>
                  </div>
                  <div className="h-1.5 overflow-hidden rounded-full bg-secondary">
                    <div
                      className={`h-full rounded-full transition-all ${toneBarClass(item.tone)}`}
                      style={{ width: `${pct}%` }}
                    />
                  </div>
                  {item.detail && (
                    <div className="truncate font-mono text-[10px] text-faint">{item.detail}</div>
                  )}
                </div>
              );
            })}
          </div>
        </div>
      );
    },

    KeyValueList: ({ props }) => <KeyValueListView items={props.items ?? []} />,

    Callout: ({ props }) => {
      const Icon =
        props.tone === "critical"
          ? AlertCircle
          : props.tone === "warning"
            ? AlertTriangle
            : props.tone === "good"
              ? CheckCircle2
              : Info;
      const borderClass =
        props.tone === "critical"
          ? "border-critical/45 bg-[color-mix(in_oklab,var(--critical)_8%,var(--card))] text-critical"
          : props.tone === "warning"
            ? "border-warning/45 bg-[color-mix(in_oklab,var(--warning)_10%,var(--card))] text-warning-ink"
            : props.tone === "good"
              ? "border-good/40 bg-[color-mix(in_oklab,var(--good)_8%,var(--card))] text-good-ink"
              : "border-frost/40 bg-[color-mix(in_oklab,var(--frost)_8%,var(--card))] text-frost";
      return (
        <Alert className={`rounded-xl ${borderClass}`}>
          <Icon className="size-4" />
          <AlertTitle className="text-[12.5px] font-semibold">{props.title}</AlertTitle>
          <AlertDescription className="text-[12px] text-foreground/90">{props.message}</AlertDescription>
        </Alert>
      );
    },

    Badge: ({ props }) => (
      <UiBadge variant="outline" className={`font-mono text-[10.5px] ${toneBadgeClass(props.tone)}`}>
        {props.label}
      </UiBadge>
    ),

    SqlBlock: ({ props }) => <SqlBlockView props={props} />,

    CodeBlock: ({ props }) => <CodeBlockView props={props} />,

    Text: ({ props }) => {
      if (props.variant === "heading") {
        return <h5 className="text-[13.5px] font-semibold text-foreground">{props.content}</h5>;
      }
      if (props.variant === "mono") {
        return <p className="font-mono text-[11.5px] text-foreground">{props.content}</p>;
      }
      if (props.variant === "muted") {
        return <p className="text-[12px] text-muted-foreground">{props.content}</p>;
      }
      return <p className="text-[13px] leading-relaxed text-foreground">{props.content}</p>;
    },

    ActionButton: ({ props, emit }) => (
      <Button
        type="button"
        size="sm"
        variant={props.variant ?? "outline"}
        className="h-7 rounded-lg font-mono text-[11px]"
        onClick={() => emit("press")}
      >
        {props.label}
      </Button>
    ),
  },
  actions: {
    askFollowUp: async () => {},
    navigateTo: async () => {},
    copyText: async (params) => {
      if (params?.text && typeof navigator !== "undefined") {
        await navigator.clipboard?.writeText(String(params.text));
      }
    },
  },
});

export { DataTableView, SqlBlockView };
