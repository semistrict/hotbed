import { useMemo, useState } from "react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { commandSummary, shellJoinWords, shellQuoteWord, unwrapCommand } from "../lib/command";
import { CodeLines, useHighlighter } from "./CodeLines";
import { CopyButton } from "./CopyButton";

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

/**
 * Full command display for the Command detail page and Schedule cards.
 *
 * Short single-line commands render cleanly inline; inline `bash -c` and
 * `python(3) -c` scripts and long/multi-line commands render in a
 * syntax-highlighted code box with line numbers, a Script/Raw toggle,
 * verbatim Copy button, and collapse/expand when tall. A one-line shell
 * script shows one command per line in the Script view; Raw and Copy keep
 * it as run.
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
  const highlighter = useHighlighter();

  const isScript = parsed.kind === "script";
  const formatted = useMemo(
    () => (parsed.kind === "script" && parsed.language === "bash" ? highlighter?.formatOneLiner(parsed.display) : undefined),
    [highlighter, parsed],
  );
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
      <div className="mb-4 flex items-center gap-2 rounded-lg border border-border bg-muted/50 px-3 py-2 font-mono text-[11.5px]">
        <code className="min-w-0 flex-1 overflow-x-auto whitespace-pre text-foreground">
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

  const scriptView = isScript && !rawView;
  const displayCode = scriptView ? (formatted ?? parsed.display) : parsed.raw;
  const displayLanguage = scriptView ? parsed.language : "bash";
  const totalLines = displayCode.split("\n").length;
  const collapsible = totalLines > maxLines;
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
        {variant === "hero" ? (
          <h1 className="flex min-w-0 items-center gap-1.5 text-[12px] font-medium text-muted-foreground">
            <span className="select-none text-primary">$</span>
            {env && env.length > 0 && <span className="truncate text-frost">{env.map(formatShellEnv).join(" ")}</span>}
            <span className="truncate text-foreground">{invocationText}</span>
          </h1>
        ) : (
          <span className="flex min-w-0 items-center gap-1.5 text-muted-foreground">
            <span className="select-none text-primary">$</span>
            {env && env.length > 0 && <span className="truncate text-frost">{env.map(formatShellEnv).join(" ")}</span>}
            <span className="truncate text-foreground">{invocationText}</span>
          </span>
        )}
        <Badge variant="secondary" className={`h-auto rounded px-1.5 py-0.5 text-[10px] font-medium ${langTone}`}>
          {langLabel}
        </Badge>
        {isScript && parsed.trailingArgs.length > 0 && (
          <span className="truncate text-[10.5px] text-faint" title={`Arguments: ${shellJoinWords(parsed.trailingArgs)}`}>
            args: {shellJoinWords(parsed.trailingArgs)}
          </span>
        )}
        <span className="text-[10.5px] text-faint">
          {totalLines} {totalLines === 1 ? "line" : "lines"}
          {scriptView && formatted !== undefined && <span title="Laid out one command per line; Raw shows it as run"> · formatted</span>}
        </span>
        <div className="ml-auto flex shrink-0 items-center gap-1.5">
          {isScript && (
            <Tabs value={rawView ? "raw" : "script"} onValueChange={(v) => setRawView(v === "raw")}>
              <TabsList className="h-6 border border-border bg-background/50 p-0.5">
                <TabsTrigger value="script" className="px-1.5 py-0 font-mono text-[10px]">
                  Script
                </TabsTrigger>
                <TabsTrigger value="raw" className="px-1.5 py-0 font-mono text-[10px]">
                  Raw
                </TabsTrigger>
              </TabsList>
            </Tabs>
          )}
          <CopyButton text={copyText} label={copyLabel} />
        </div>
      </div>

      <div className="relative">
        <CodeLines
          code={displayCode}
          language={displayLanguage}
          gutter={showGutter}
          limit={collapsible && !expanded ? maxLines - 2 : undefined}
          className="py-2.5 text-[12px]"
          lineClassName="pr-3"
        />

        {collapsible && !expanded && (
          <div
            className={`pointer-events-none absolute inset-x-0 bottom-0 h-10 bg-gradient-to-t to-transparent ${
              variant === "hero" ? "from-card/95" : "from-muted/95"
            }`}
          />
        )}
      </div>

      {collapsible && (
        <div className="border-t border-border bg-muted/25 px-3 py-1 text-center">
          <Button
            type="button"
            variant="ghost"
            size="xs"
            onClick={() => setExpanded((e) => !e)}
            className="font-mono text-[10.5px] text-muted-foreground hover:text-primary"
          >
            {expanded ? "Collapse" : `Show all ${totalLines} lines`}
          </Button>
        </div>
      )}
    </div>
  );
}

/**
 * A bash script, such as a setup step's: syntax-highlighted with line
 * numbers, a verbatim Copy button, and collapsed past maxLines.
 */
export function ScriptBlock({ script, maxLines = 12 }: { script: string; maxLines?: number }) {
  const [expanded, setExpanded] = useState(false);
  const code = script.replace(/\n$/, "");
  const totalLines = code.split("\n").length;
  const collapsible = totalLines > maxLines;
  return (
    <div className="overflow-hidden rounded-lg border border-border bg-muted/40">
      <div className="relative">
        <div className="absolute top-1.5 right-1.5 z-10">
          <CopyButton text={script} label="Copy" />
        </div>
        <CodeLines
          code={code}
          language="bash"
          gutter
          limit={collapsible && !expanded ? maxLines - 2 : undefined}
          className="py-2 text-[11.5px]"
          lineClassName="pr-16"
        />
      </div>
      {collapsible && (
        <div className="border-t border-border bg-muted/25 px-3 py-1 text-center">
          <Button
            type="button"
            variant="ghost"
            size="xs"
            onClick={() => setExpanded((e) => !e)}
            className="font-mono text-[10.5px] text-muted-foreground hover:text-primary"
          >
            {expanded ? "Collapse" : `Show all ${totalLines} lines`}
          </Button>
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
        <Badge
          variant="secondary"
          className={`h-auto shrink-0 rounded px-1 py-px font-mono text-[9.5px] leading-tight font-normal ${
            badge === "python" ? "bg-frost-wash text-frost" : "bg-ember-wash text-primary"
          }`}
        >
          {badge}
        </Badge>
      )}
      <span className="truncate">{text}</span>
    </span>
  );
}
