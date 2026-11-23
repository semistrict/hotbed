import { useMemo, useState } from "react";
import { commandSummary, shellJoinWords, shellQuoteWord, unwrapCommand } from "../lib/command";

const SHELL_VAR_RE = /^[A-Za-z_][A-Za-z0-9_]*$/;

/** Format a single `KEY=VALUE` environment entry as a POSIX shell assignment. */
export function formatShellEnv(entry: string): string {
  const eq = entry.indexOf("=");
  if (eq <= 0) return shellQuoteWord(entry);
  const key = entry.slice(0, eq);
  if (!SHELL_VAR_RE.test(key)) return shellQuoteWord(entry);
  const val = entry.slice(eq + 1);
  return `${key}=${shellQuoteWord(val)}`;
}

/** Prepend shell-quoted environment assignments to a raw command line. */
export function formatRawWithEnv(raw: string, env?: readonly string[]): string {
  if (!env || env.length === 0) return raw;
  const prefix = env.map(formatShellEnv).join(" ");
  return raw ? `${prefix} ${raw}` : prefix;
}

function CopyButton({ text, label = "Copy" }: { text: string; label?: string }) {
  const [copied, setCopied] = useState(false);

  const onCopy = () => {
    if (typeof navigator === "undefined" || !navigator.clipboard) return;
    void navigator.clipboard.writeText(text).then(() => {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    });
  };

  return (
    <button
      type="button"
      onClick={onCopy}
      className="rounded-md border border-border px-2 py-0.5 font-mono text-[10.5px] text-muted-foreground transition-colors hover:border-border-strong hover:text-foreground"
      title={`Copy ${label.toLowerCase()} to clipboard`}
    >
      {copied ? "✓ Copied" : label}
    </button>
  );
}

/**
 * Full command display for the Command detail page and Schedule cards.
 *
 * Short single-line commands render cleanly inline; inline `bash -c` and
 * `python(3) -c` scripts and long/multi-line commands render in a monospace
 * code box with line numbers, a Script/Raw toggle, verbatim Copy button,
 * and collapse/expand when tall.
 */
export function CommandBlock({
  argv,
  args,
  env,
  variant = "hero",
  maxLines = 16,
}: {
  argv?: string;
  args?: readonly string[];
  env?: string[];
  variant?: "hero" | "compact";
  maxLines?: number;
}) {
  const parsed = useMemo(() => unwrapCommand(args, argv), [args, argv]);
  const [rawView, setRawView] = useState(false);
  const [expanded, setExpanded] = useState(false);

  const isScript = parsed.kind === "script";
  const rawWithEnv = formatRawWithEnv(parsed.raw, env);

  // Short plain commands on the Command page render as a crisp monospace heading.
  if (variant === "hero" && parsed.kind === "command" && parsed.compact && (!env || env.length === 0)) {
    return (
      <div className="mt-3 flex items-start gap-3">
        <h1 className="min-w-0 flex-1 break-words font-mono text-[17px] leading-snug text-foreground sm:text-[20px]">
          <span className="select-none text-primary">$ </span>
          <code>{parsed.raw}</code>
        </h1>
        <div className="shrink-0 pt-0.5">
          <CopyButton text={parsed.raw} />
        </div>
      </div>
    );
  }

  // Short plain commands inside Schedule cards stay in a slim single-row box.
  if (variant === "compact" && parsed.kind === "command" && parsed.compact) {
    return (
      <div className="mb-4 flex items-center gap-2 overflow-x-auto rounded-lg border border-border bg-muted/50 px-3 py-2 font-mono text-[11.5px]">
        <code className="min-w-0 flex-1 whitespace-pre text-foreground">
          {env?.map((e, idx) => (
            <span key={`${idx}-${e}`} className="mr-1.5 text-frost">
              {formatShellEnv(e)}
            </span>
          ))}
          {parsed.raw}
        </code>
        <div className="shrink-0">
          <CopyButton text={rawWithEnv} />
        </div>
      </div>
    );
  }

  const displayCode = isScript && !rawView ? parsed.display : parsed.raw;
  const lines = displayCode.split("\n");
  const totalLines = lines.length;
  const collapsible = totalLines > maxLines;
  const shownLines = collapsible && !expanded ? lines.slice(0, maxLines - 2) : lines;
  const showGutter = totalLines > 1 || isScript;

  const langLabel = isScript ? parsed.badge : "shell";
  const langTone =
    isScript && parsed.language === "python"
      ? "bg-frost-wash text-frost"
      : isScript
        ? "bg-ember-wash text-primary"
        : "bg-muted text-muted-foreground";

  const invocationText = isScript ? shellJoinWords(parsed.prefix) : "command";
  // Always copy exact verbatim script (when unwrapped and in script view) or verbatim raw command.
  const copyText = isScript && !rawView ? parsed.script : rawWithEnv;
  const copyLabel = isScript && !rawView ? "Copy script" : rawView ? "Copy raw" : "Copy";

  return (
    <div
      className={
        variant === "hero"
          ? "mt-3 overflow-hidden rounded-xl border border-border bg-card/85 shadow-[0_1px_0_0_var(--border)_inset]"
          : "mb-4 overflow-hidden rounded-lg border border-border bg-muted/40"
      }
    >
      <div className="flex flex-wrap items-center gap-x-2.5 gap-y-1.5 border-b border-border bg-muted/45 px-3 py-2 font-mono text-[11px]">
        <span className="flex min-w-0 items-center gap-1.5 text-muted-foreground">
          <span className="select-none text-primary">$</span>
          {env && env.length > 0 && <span className="truncate text-frost">{env.map(formatShellEnv).join(" ")}</span>}
          <span className="truncate text-foreground">{invocationText}</span>
        </span>
        <span className={`rounded px-1.5 py-0.5 text-[10px] font-medium ${langTone}`}>{langLabel}</span>
        {isScript && parsed.trailingArgs.length > 0 && (
          <span className="truncate text-[10.5px] text-faint" title={`Arguments: ${shellJoinWords(parsed.trailingArgs)}`}>
            args: {shellJoinWords(parsed.trailingArgs)}
          </span>
        )}
        <span className="text-[10.5px] text-faint">
          {totalLines} {totalLines === 1 ? "line" : "lines"}
        </span>
        <div className="ml-auto flex shrink-0 items-center gap-1.5">
          {isScript && (
            <div className="inline-flex rounded-md border border-border bg-background/50 p-0.5 text-[10px]">
              <button
                type="button"
                onClick={() => setRawView(false)}
                className={`rounded px-1.5 py-0.5 transition-colors ${!rawView ? "bg-muted text-foreground" : "text-muted-foreground hover:text-foreground"}`}
              >
                Script
              </button>
              <button
                type="button"
                onClick={() => setRawView(true)}
                className={`rounded px-1.5 py-0.5 transition-colors ${rawView ? "bg-muted text-foreground" : "text-muted-foreground hover:text-foreground"}`}
              >
                Raw
              </button>
            </div>
          )}
          <CopyButton text={copyText} label={copyLabel} />
        </div>
      </div>

      <div className="relative">
        <pre className="m-0 overflow-x-auto py-2.5 font-mono text-[12px] leading-[1.65] text-foreground">
          <code>
            {shownLines.map((line, idx) => (
              <span key={idx} className="flex hover:bg-muted/35">
                {showGutter && (
                  <span className="w-10 shrink-0 pr-3 text-right text-[10.5px] leading-[1.9] select-none text-faint">
                    {idx + 1}
                  </span>
                )}
                <span className={`min-w-0 flex-1 pr-3 whitespace-pre-wrap break-words ${showGutter ? "" : "pl-3"}`}>
                  {line || " "}
                </span>
              </span>
            ))}
          </code>
        </pre>

        {collapsible && !expanded && (
          <div className="pointer-events-none absolute inset-x-0 bottom-0 h-10 bg-gradient-to-t from-card/95 to-transparent" />
        )}
      </div>

      {collapsible && (
        <div className="border-t border-border bg-muted/25 px-3 py-1.5 text-center">
          <button
            type="button"
            onClick={() => setExpanded((e) => !e)}
            className="font-mono text-[10.5px] text-muted-foreground transition-colors hover:text-primary"
          >
            {expanded ? "Collapse" : `Show all ${totalLines} lines`}
          </button>
        </div>
      )}
    </div>
  );
}

/**
 * A bash script, such as a setup step's: monospace with line numbers,
 * a verbatim Copy button, and collapsed past maxLines.
 */
export function ScriptBlock({ script, maxLines = 12 }: { script: string; maxLines?: number }) {
  const [expanded, setExpanded] = useState(false);
  const lines = script.replace(/\n$/, "").split("\n");
  const collapsible = lines.length > maxLines;
  const shown = collapsible && !expanded ? lines.slice(0, maxLines - 2) : lines;
  return (
    <div className="overflow-hidden rounded-lg border border-border bg-muted/40">
      <div className="relative">
        <div className="absolute top-1.5 right-1.5 z-10">
          <CopyButton text={script} label="Copy" />
        </div>
        <pre className="m-0 overflow-x-auto py-2 font-mono text-[11.5px] leading-[1.65] text-foreground">
          <code>
            {shown.map((line, i) => (
              <span key={i} className="flex hover:bg-muted/35">
                <span className="w-9 shrink-0 pr-3 text-right text-[10px] leading-[1.9] select-none text-faint">{i + 1}</span>
                <span className="min-w-0 flex-1 pr-16 whitespace-pre-wrap break-words">{line || " "}</span>
              </span>
            ))}
          </code>
        </pre>
      </div>
      {collapsible && (
        <div className="border-t border-border bg-muted/25 px-3 py-1 text-center">
          <button type="button" onClick={() => setExpanded((e) => !e)} className="font-mono text-[10.5px] text-muted-foreground transition-colors hover:text-primary">
            {expanded ? "Collapse" : `Show all ${lines.length} lines`}
          </button>
        </div>
      )}
    </div>
  );
}

/**
 * Single-line command summary for tables and chart rows.
 * Shows a subtle language pill (`bash` / `python`) when the command is an
 * inline script, followed by a clean preview instead of raw `bash -c '...'`.
 */
export function CommandInline({ argv, args, className = "" }: { argv: string; args?: readonly string[]; className?: string }) {
  const { badge, text } = useMemo(() => commandSummary(argv, args), [argv, args]);
  return (
    <span className={`inline-flex max-w-full min-w-0 items-center gap-1.5 align-bottom ${className}`} title={argv}>
      {badge && (
        <span
          className={`shrink-0 rounded px-1 py-px font-mono text-[9.5px] leading-tight ${
            badge === "python" ? "bg-frost-wash text-frost" : "bg-ember-wash text-primary"
          }`}
        >
          {badge}
        </span>
      )}
      <span className="truncate">{text}</span>
    </span>
  );
}
