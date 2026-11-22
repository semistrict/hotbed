import { useMemo, useState } from "react";
import { commandSummary, parseCommand, shellJoinWords } from "../lib/command";
import { tokenize, type Token, type TokenKind } from "../lib/highlight";

const TOKEN_STYLE: Record<TokenKind, string> = {
  plain: "text-foreground",
  keyword: "text-[var(--syn-keyword)] font-medium",
  builtin: "text-[var(--syn-builtin)]",
  string: "text-[var(--syn-string)]",
  comment: "text-[var(--syn-comment)] italic",
  variable: "text-[var(--syn-variable)]",
  number: "text-[var(--syn-number)]",
  operator: "text-[var(--syn-operator)]",
  flag: "text-[var(--syn-flag)]",
  command: "text-foreground font-semibold",
  function: "text-[var(--syn-function)]",
  decorator: "text-[var(--syn-decorator)]",
};

function HighlightedLine({ tokens }: { tokens: Token[] }) {
  if (tokens.length === 0) return <> </>;
  return (
    <>
      {tokens.map((t, i) => (
        <span key={i} className={TOKEN_STYLE[t.kind]}>
          {t.text}
        </span>
      ))}
    </>
  );
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
 * `python(3) -c` scripts and long/multi-line commands render in a compact,
 * syntax-highlighted code box with line numbers, a Formatted/Raw toggle,
 * Copy button, and collapse/expand when tall.
 */
export function CommandBlock({
  argv,
  env,
  variant = "hero",
  maxLines = 16,
}: {
  argv: string;
  env?: string[];
  variant?: "hero" | "compact";
  maxLines?: number;
}) {
  const parsed = useMemo(() => parseCommand(argv), [argv]);
  const [rawView, setRawView] = useState(false);
  const [expanded, setExpanded] = useState(false);

  const isScript = parsed.kind === "script";
  const canToggleRaw = isScript || parsed.formatted !== parsed.raw;

  // Short plain commands on the Command page render as a crisp, modestly-sized
  // monospace heading instead of an oversized 32px banner or heavy box.
  if (variant === "hero" && parsed.kind === "command" && parsed.compact && (!env || env.length === 0)) {
    const tokens = tokenize(parsed.formatted, "bash")[0] ?? [];
    return (
      <div className="mt-3 flex items-start gap-3">
        <h1 className="min-w-0 flex-1 break-words font-mono text-[17px] leading-snug text-foreground sm:text-[20px]">
          <span className="select-none text-primary">$ </span>
          <HighlightedLine tokens={tokens} />
        </h1>
        <div className="shrink-0 pt-0.5">
          <CopyButton text={argv} />
        </div>
      </div>
    );
  }

  // Short plain commands inside Schedule cards stay in a slim single-row box.
  if (variant === "compact" && parsed.kind === "command" && parsed.compact) {
    const tokens = tokenize(parsed.formatted, "bash")[0] ?? [];
    return (
      <div className="mb-4 flex items-center gap-2 overflow-x-auto rounded-lg border border-border bg-muted/50 px-3 py-2 font-mono text-[11.5px]">
        <div className="min-w-0 flex-1 whitespace-pre">
          {env?.map((e) => (
            <span key={e} className="mr-1.5 text-[var(--syn-variable)]">
              {e}
            </span>
          ))}
          <HighlightedLine tokens={tokens} />
        </div>
        <div className="shrink-0">
          <CopyButton text={env && env.length > 0 ? `${env.join(" ")} ${argv}` : argv} />
        </div>
      </div>
    );
  }

  const displayCode = rawView ? parsed.raw : parsed.formatted;
  const displayLang = rawView ? "bash" : parsed.language;
  const tokenLines = tokenize(displayCode, displayLang);
  const totalLines = tokenLines.length;
  const collapsible = totalLines > maxLines;
  const shownLines = collapsible && !expanded ? tokenLines.slice(0, maxLines - 2) : tokenLines;
  const showGutter = totalLines > 1 || isScript;

  const langLabel = isScript ? parsed.interpreter : "shell";
  const langTone =
    isScript && parsed.language === "python"
      ? "bg-frost-wash text-frost"
      : isScript
        ? "bg-ember-wash text-primary"
        : "bg-muted text-muted-foreground";

  const invocationText = isScript ? shellJoinWords(parsed.prefix) : "command";
  const copyText = rawView ? parsed.raw : isScript ? parsed.formatted : parsed.raw;
  const copyLabel = rawView ? "Copy raw" : isScript ? "Copy script" : "Copy";

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
          {env && env.length > 0 && (
            <span className="truncate text-[var(--syn-variable)]">{env.join(" ")}</span>
          )}
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
          {canToggleRaw && (
            <div className="inline-flex rounded-md border border-border bg-background/50 p-0.5 text-[10px]">
              <button
                type="button"
                onClick={() => setRawView(false)}
                className={`rounded px-1.5 py-0.5 transition-colors ${!rawView ? "bg-muted text-foreground" : "text-muted-foreground hover:text-foreground"}`}
              >
                Formatted
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
        <div className="overflow-x-auto py-2.5 font-mono text-[12px] leading-[1.65]">
          {shownLines.map((lineTokens, idx) => (
            <div key={idx} className="flex hover:bg-muted/35">
              {showGutter && (
                <span className="w-10 shrink-0 pr-3 text-right text-[10.5px] leading-[1.9] select-none text-faint">
                  {idx + 1}
                </span>
              )}
              <span className={`min-w-0 flex-1 pr-3 whitespace-pre-wrap break-words ${showGutter ? "" : "pl-3"}`}>
                <HighlightedLine tokens={lineTokens} />
              </span>
            </div>
          ))}
        </div>

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
 * Single-line command summary for tables and chart rows.
 * Shows a subtle language pill (`bash` / `py`) when the command is an inline
 * script, followed by a clean unescaped preview instead of raw `bash -c '...'`.
 */
export function CommandInline({ argv, className = "" }: { argv: string; className?: string }) {
  const { badge, text } = useMemo(() => commandSummary(argv), [argv]);
  return (
    <span className={`inline-flex max-w-full min-w-0 items-center gap-1.5 align-bottom ${className}`} title={argv}>
      {badge && (
        <span
          className={`shrink-0 rounded px-1 py-px font-mono text-[9.5px] leading-tight ${
            badge === "py" ? "bg-frost-wash text-frost" : "bg-ember-wash text-primary"
          }`}
        >
          {badge}
        </span>
      )}
      <span className="truncate">{text}</span>
    </span>
  );
}
