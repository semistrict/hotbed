// Unwrap inline shell/Python scripts from structured argv arrays and join argv
// words as POSIX shell syntax. Never rewrites or reformats user code.

export type ScriptLanguage = "bash" | "python";

export type ParsedCommand =
  | {
      kind: "script";
      language: ScriptLanguage;
      badge: string;
      interpreter: string;
      prefix: string[];
      /** Exact verbatim script from the `-c` argument (used for copying). */
      script: string;
      /** Dedented script for display (outer blank lines and common indent trimmed). */
      display: string;
      trailingArgs: string[];
      raw: string;
    }
  | { kind: "command"; raw: string; compact: boolean };

const SHELL_NAMES = new Set(["sh", "bash", "zsh", "dash", "ash", "ksh", "mksh"]);
const PYTHON_RE = /^(python(\d+(\.\d+)*)?|pypy3?)$/;
const ENV_ASSIGN_RE = /^[A-Za-z_][A-Za-z0-9_]*=/;

interface WrapperSpec {
  argFlags: Set<string>;
  envAssign?: boolean;
  trailing?: number;
}

const WRAPPER_OPTS = new Map<string, WrapperSpec>([
  ["env", { argFlags: new Set(["-u", "-C", "--unset", "--chdir"]), envAssign: true }],
  ["sudo", { argFlags: new Set(["-u", "-g", "-C"]) }],
  ["timeout", { argFlags: new Set(["-k", "-s", "--kill-after", "--signal"]), trailing: 1 }],
  ["nice", { argFlags: new Set(["-n", "--adjustment"]) }],
  ["nohup", { argFlags: new Set() }],
  ["setsid", { argFlags: new Set() }],
  ["time", { argFlags: new Set() }],
]);
const UV_RUN_SPEC: WrapperSpec = { argFlags: new Set(["--with", "--python", "-p", "--directory", "--project"]) };

/** Quote a single word as POSIX shell syntax, matching Go's `shell.Join`. */
export function shellQuoteWord(word: string): string {
  return /^[\w@%+=:,./-]+$/.test(word) ? word : `'${word.replaceAll("'", `'\\''`)}'`;
}

/** Join words as a POSIX shell command line. */
export function shellJoinWords(words: readonly string[]): string {
  return words.map(shellQuoteWord).join(" ");
}

const basename = (p: string) => p.slice(p.lastIndexOf("/") + 1);

/** Trim outer blank lines and uniform common leading whitespace for display. */
export function dedent(source: string): string {
  const lines = source.replace(/\r\n?/g, "\n").split("\n");
  let s = 0;
  let e = lines.length;
  while (s < e && lines[s]!.trim() === "") s++;
  while (e > s && lines[e - 1]!.trim() === "") e--;
  const trimmed = lines.slice(s, e);
  const indents = trimmed.filter((l) => l.trim() !== "").map((l) => l.match(/^[ \t]*/)?.[0].length ?? 0);
  const drop = indents.length > 0 ? Math.min(...indents) : 0;
  return trimmed.map((l) => l.slice(drop).replace(/[ \t]+$/, "")).join("\n");
}

function peelWrappers(words: readonly string[]): { prefix: string[]; index: number } {
  const prefix: string[] = [];
  let i = 0;
  const take = (n = 1) => {
    for (let k = 0; k < n && i < words.length; k++) prefix.push(words[i++]!);
  };
  while (i < words.length) {
    const w = words[i]!;
    if (ENV_ASSIGN_RE.test(w)) {
      take();
      continue;
    }
    const base = basename(w);
    const wrapper = WRAPPER_OPTS.get(base);
    const spec =
      base === "uv" && words[i + 1] === "run"
        ? (take(2), UV_RUN_SPEC)
        : wrapper
          ? (take(), wrapper)
          : null;
    if (!spec) break;
    while (i < words.length) {
      const a = words[i]!;
      if (a === "--") {
        take();
        break;
      }
      if ((spec.envAssign && ENV_ASSIGN_RE.test(a)) || (a.startsWith("-") && !spec.argFlags.has(a))) {
        take();
        continue;
      }
      if (spec.argFlags.has(a)) {
        take(2);
        continue;
      }
      break;
    }
    if (spec.trailing) take(spec.trailing);
  }
  return { prefix, index: i };
}

/**
 * Unwrap inline shell (`bash -lc`, `sh -euo pipefail -c`) or Python (`python3 -c`)
 * invocations from `args` without rewriting code; falls back to verbatim `rawArgv`.
 */
export function unwrapCommand(args?: readonly string[], rawArgv?: string): ParsedCommand {
  const raw = rawArgv || (args && args.length > 0 ? shellJoinWords(args) : "");
  if (args && args.length >= 2) {
    const { prefix, index } = peelWrappers(args);
    const bin = args[index];
    const base = bin ? basename(bin) : "";
    const isShell = SHELL_NAMES.has(base);
    const isPython = !isShell && PYTHON_RE.test(base);
    if (bin && (isShell || isPython)) {
      const cmdPrefix = [...prefix, bin];
      for (let j = index + 1; j < args.length; ) {
        const arg = args[j]!;
        if (arg === "--") break;
        if (isShell) {
          if (arg === "-o" || arg === "+o" || arg === "-O" || arg === "+O") {
            cmdPrefix.push(arg);
            if (++j < args.length) cmdPrefix.push(args[j++]!);
          } else if (arg.startsWith("--")) {
            cmdPrefix.push(arg);
            j++;
          } else if (/^[-+][a-zA-Z]+$/.test(arg)) {
            cmdPrefix.push(arg);
            j++;
            for (const c of arg.slice(1)) if ((c === "o" || c === "O") && j < args.length) cmdPrefix.push(args[j++]!);
            if (arg.startsWith("-") && arg.includes("c") && j < args.length) {
              const script = args[j]!;
              return { kind: "script", language: "bash", badge: base, interpreter: base, prefix: cmdPrefix, script, display: dedent(script), trailingArgs: args.slice(j + 1), raw };
            }
          } else break;
        } else {
          if ((arg === "-c" || /^-[uBEIOPsvqidbx3]+c$/.test(arg)) && j + 1 < args.length) {
            cmdPrefix.push(arg);
            const script = args[j + 1]!;
            return { kind: "script", language: "python", badge: "python", interpreter: base, prefix: cmdPrefix, script, display: dedent(script), trailingArgs: args.slice(j + 2), raw };
          }
          if (arg.startsWith("-c") && arg.length > 2) {
            cmdPrefix.push("-c");
            const script = arg.slice(2);
            return { kind: "script", language: "python", badge: "python", interpreter: base, prefix: cmdPrefix, script, display: dedent(script), trailingArgs: args.slice(j + 1), raw };
          }
          if (arg === "-W" || arg === "-X" || arg === "--check-hash-based-pycs") {
            cmdPrefix.push(arg);
            if (++j < args.length) cmdPrefix.push(args[j++]!);
          } else if (arg.startsWith("-W") || arg.startsWith("-X") || arg.startsWith("--check-hash-based-pycs=") || /^-[uBEIOPsvqidbx3]+$/.test(arg)) {
            cmdPrefix.push(arg);
            j++;
          } else break;
        }
      }
    }
  }
  return { kind: "command", raw, compact: !raw.includes("\n") && raw.length <= 100 };
}

/** Compact one-line summary for narrow table cells and bars. */
export function commandSummary(argv: string, args?: readonly string[]): { badge: string | null; text: string } {
  const parsed = unwrapCommand(args, argv);
  if (parsed.kind === "script") {
    const lines = parsed.display.split("\n").map((l) => l.trim()).filter((l) => l.length > 0 && !l.startsWith("#"));
    return { badge: parsed.badge, text: ((lines.length > 0 ? lines.join(" ; ") : parsed.script.trim()).replace(/\s+/g, " ")) || parsed.raw };
  }
  return { badge: null, text: parsed.raw.replace(/\s+/g, " ").trim() };
}
