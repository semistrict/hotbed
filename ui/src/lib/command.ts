// Parse, unwrap, format, and summarize commands that agents run on hotbed.
//
// Commands arrive over the API as POSIX shell-quoted strings (`shell.Join` on
// the worker, which preserves newlines inside single quotes). Agents frequently
// invoke `bash -c '<multiline script>'` or `python3 -c '<script>'`, sometimes
// behind wrappers like `env`, `timeout`, or `uv run`. Unwrapping those restores
// newlines and unescapes quotes so the UI can render formatted, highlighted code
// instead of a giant single-line wall of text.

export type ScriptLanguage = "bash" | "python";

export type ParsedCommand =
  | {
      kind: "script";
      language: ScriptLanguage;
      /** Display name of the interpreter, e.g. "bash", "sh", "python3". */
      interpreter: string;
      /** Wrapper + interpreter + flags before the inline script, e.g. ["bash", "-c"]. */
      prefix: string[];
      /** Unquoted inline script source, verbatim from the argument. */
      script: string;
      /** Formatted script ready for display. */
      formatted: string;
      /** Extra positional arguments passed after the inline script ($0/$1 or sys.argv[1:]). */
      trailingArgs: string[];
      /** Original shell-quoted argv string. */
      raw: string;
    }
  | {
      kind: "command";
      language: "bash";
      /** Formatted command line (single line if short, multi-line if long/multiline). */
      formatted: string;
      /** Original shell-quoted argv string. */
      raw: string;
      /** True for short single-line commands that read best inline. */
      compact: boolean;
    };

const SHELL_NAMES = new Set(["sh", "bash", "zsh", "dash", "ash", "ksh", "mksh"]);
const PYTHON_RE = /^(python(\d+(\.\d+)*)?|pypy3?)$/;
const ENV_ASSIGN_RE = /^[A-Za-z_][A-Za-z0-9_]*=/;

/**
 * Split a POSIX shell-quoted command string into words.
 *
 * Losslessly round-trips `shell.Join` (including newlines, empty strings `''`,
 * and escaped single quotes `'\''`) and handles double quotes and backslash
 * escapes. Returns null if quotes are unbalanced.
 */
export function splitShellWords(input: string): string[] | null {
  const words: string[] = [];
  let cur = "";
  let hasToken = false;
  let i = 0;
  const n = input.length;

  while (i < n) {
    const ch = input[i]!;
    if (ch === " " || ch === "\t" || ch === "\n" || ch === "\r") {
      if (hasToken) {
        words.push(cur);
        cur = "";
        hasToken = false;
      }
      i++;
      continue;
    }

    hasToken = true;
    if (ch === "'") {
      i++;
      const start = i;
      const close = input.indexOf("'", start);
      if (close < 0) return null;
      cur += input.slice(start, close);
      i = close + 1;
    } else if (ch === '"') {
      i++;
      let closed = false;
      while (i < n) {
        const c = input[i]!;
        if (c === '"') {
          i++;
          closed = true;
          break;
        }
        if (c === "\\" && i + 1 < n) {
          const next = input[i + 1]!;
          if (next === '"' || next === "\\" || next === "$" || next === "`") {
            cur += next;
            i += 2;
            continue;
          }
          if (next === "\n") {
            i += 2;
            continue;
          }
        }
        cur += c;
        i++;
      }
      if (!closed) return null;
    } else if (ch === "\\" && i + 1 < n) {
      const next = input[i + 1]!;
      if (next === "\n") {
        i += 2;
      } else {
        cur += next;
        i += 2;
      }
    } else {
      cur += ch;
      i++;
    }
  }

  if (hasToken) words.push(cur);
  return words;
}

/** Quote a single word as POSIX shell syntax, matching Go's `shell.Join`. */
export function shellQuoteWord(word: string): string {
  return /^[\w@%+=:,./-]+$/.test(word) ? word : `'${word.replaceAll("'", `'\\''`)}'`;
}

/** Join words as a POSIX shell command line. */
export function shellJoinWords(words: readonly string[]): string {
  return words.map(shellQuoteWord).join(" ");
}

function basename(path: string): string {
  const idx = path.lastIndexOf("/");
  return idx >= 0 ? path.slice(idx + 1) : path;
}

/**
 * Parse a command string (and optional raw argv array) into either an unwrapped
 * inline bash/python script or a regular command.
 */
export function parseCommand(argv: string, explicitArgs?: readonly string[]): ParsedCommand {
  const words = explicitArgs && explicitArgs.length > 0 ? [...explicitArgs] : splitShellWords(argv);
  if (words && words.length >= 2) {
    const script = extractInlineScript(words, argv);
    if (script) return script;
  }

  const formatted = formatPlainCommand(words, argv);
  const compact = !formatted.includes("\n") && formatted.length <= 100;
  return { kind: "command", language: "bash", formatted, raw: argv, compact };
}

function extractInlineScript(words: string[], raw: string): Extract<ParsedCommand, { kind: "script" }> | null {
  const prefix: string[] = [];
  let i = 0;

  // Peel common leading wrappers: VAR=val, env, timeout, nice, stdbuf, sudo, uv run, etc.
  while (i < words.length) {
    const w = words[i]!;
    if (ENV_ASSIGN_RE.test(w)) {
      prefix.push(w);
      i++;
      continue;
    }
    const base = basename(w);
    if (base === "env") {
      prefix.push(w);
      i++;
      while (i < words.length) {
        const arg = words[i]!;
        if (arg === "--") {
          prefix.push(arg);
          i++;
          break;
        }
        if (ENV_ASSIGN_RE.test(arg)) {
          prefix.push(arg);
          i++;
          continue;
        }
        if (arg === "-u" || arg === "-C" || arg === "--unset" || arg === "--chdir") {
          prefix.push(arg);
          if (i + 1 < words.length) {
            prefix.push(words[i + 1]!);
            i += 2;
          } else {
            i++;
          }
          continue;
        }
        if (arg.startsWith("-") && arg !== "-S") {
          prefix.push(arg);
          i++;
          continue;
        }
        break;
      }
      continue;
    }
    if (base === "timeout") {
      prefix.push(w);
      i++;
      while (i < words.length && words[i]!.startsWith("-")) {
        const arg = words[i]!;
        prefix.push(arg);
        i++;
        if (arg === "--") break;
        if ((arg === "-k" || arg === "-s" || arg === "--kill-after" || arg === "--signal") && i < words.length) {
          prefix.push(words[i]!);
          i++;
        }
      }
      if (i < words.length) {
        prefix.push(words[i]!); // duration
        i++;
      }
      continue;
    }
    if (base === "nice") {
      prefix.push(w);
      i++;
      if (i < words.length && (words[i] === "-n" || words[i] === "--adjustment")) {
        prefix.push(words[i]!);
        if (i + 1 < words.length) {
          prefix.push(words[i + 1]!);
          i += 2;
        } else {
          i++;
        }
      } else if (i < words.length && /^-(n\d+|\d+|--adjustment=)/.test(words[i]!)) {
        prefix.push(words[i]!);
        i++;
      }
      continue;
    }
    if (base === "stdbuf") {
      prefix.push(w);
      i++;
      while (i < words.length && words[i]!.startsWith("-")) {
        const arg = words[i]!;
        prefix.push(arg);
        i++;
        if ((arg === "-i" || arg === "-o" || arg === "-e") && i < words.length) {
          prefix.push(words[i]!);
          i++;
        }
      }
      continue;
    }
    if (base === "nohup" || base === "setsid" || base === "time") {
      prefix.push(w);
      i++;
      continue;
    }
    if (base === "sudo") {
      prefix.push(w);
      i++;
      while (i < words.length && words[i]!.startsWith("-")) {
        const arg = words[i]!;
        prefix.push(arg);
        i++;
        if (arg === "--") break;
        if ((arg === "-u" || arg === "-g" || arg === "-C") && i < words.length) {
          prefix.push(words[i]!);
          i++;
        }
      }
      continue;
    }
    if (base === "uv" && words[i + 1] === "run") {
      prefix.push(w, words[i + 1]!);
      i += 2;
      while (i < words.length && words[i]!.startsWith("-")) {
        const arg = words[i]!;
        prefix.push(arg);
        i++;
        if (arg === "--") break;
        if (
          (arg === "--with" || arg === "--python" || arg === "-p" || arg === "--directory" || arg === "--project") &&
          i < words.length
        ) {
          prefix.push(words[i]!);
          i++;
        }
      }
      continue;
    }
    break;
  }

  if (i >= words.length) return null;
  const bin = words[i]!;
  const base = basename(bin);

  if (SHELL_NAMES.has(base)) {
    const shellPrefix = [...prefix, bin];
    let j = i + 1;
    while (j < words.length) {
      const arg = words[j]!;
      if (arg === "--") break;
      if (arg === "-o" || arg === "+o" || arg === "-O" || arg === "+O") {
        shellPrefix.push(arg);
        if (j + 1 < words.length) {
          shellPrefix.push(words[j + 1]!);
          j += 2;
        } else {
          j++;
        }
        continue;
      }
      if (arg.startsWith("--")) {
        shellPrefix.push(arg);
        j++;
        continue;
      }
      if (/^[-+][a-zA-Z]+$/.test(arg)) {
        const isMinus = arg.startsWith("-");
        const letters = arg.slice(1);
        shellPrefix.push(arg);
        j++;
        // Each 'o' or 'O' in a flag cluster consumes one following option argument.
        const optArgs = [...letters].filter((c) => c === "o" || c === "O").length;
        for (let k = 0; k < optArgs && j < words.length; k++) {
          shellPrefix.push(words[j]!);
          j++;
        }
        if (isMinus && letters.includes("c")) {
          if (j >= words.length) return null;
          const script = words[j]!;
          return {
            kind: "script",
            language: "bash",
            interpreter: base,
            prefix: shellPrefix,
            script,
            formatted: formatBash(script),
            trailingArgs: words.slice(j + 1),
            raw,
          };
        }
        continue;
      }
      break;
    }
    return null;
  }

  if (PYTHON_RE.test(base)) {
    const pyPrefix = [...prefix, bin];
    let j = i + 1;
    while (j < words.length) {
      const arg = words[j]!;
      if (arg === "-c") {
        if (j + 1 >= words.length) return null;
        pyPrefix.push(arg);
        const script = words[j + 1]!;
        return {
          kind: "script",
          language: "python",
          interpreter: base,
          prefix: pyPrefix,
          script,
          formatted: formatPython(script),
          trailingArgs: words.slice(j + 2),
          raw,
        };
      }
      if (arg.startsWith("-c") && arg.length > 2) {
        pyPrefix.push("-c");
        const script = arg.slice(2);
        return {
          kind: "script",
          language: "python",
          interpreter: base,
          prefix: pyPrefix,
          script,
          formatted: formatPython(script),
          trailingArgs: words.slice(j + 1),
          raw,
        };
      }
      if (arg === "-W" || arg === "-X" || arg === "--check-hash-based-pycs") {
        pyPrefix.push(arg);
        if (j + 1 < words.length) {
          pyPrefix.push(words[j + 1]!);
          j += 2;
        } else {
          j++;
        }
        continue;
      }
      if (arg.startsWith("-W") || arg.startsWith("-X") || arg.startsWith("--check-hash-based-pycs=")) {
        pyPrefix.push(arg);
        j++;
        continue;
      }
      if (/^-[uBEIOPsvqidbx3]+c$/.test(arg)) {
        if (j + 1 >= words.length) return null;
        pyPrefix.push(arg);
        const script = words[j + 1]!;
        return {
          kind: "script",
          language: "python",
          interpreter: base,
          prefix: pyPrefix,
          script,
          formatted: formatPython(script),
          trailingArgs: words.slice(j + 2),
          raw,
        };
      }
      if (/^-[uBEIOPsvqidbx3]+$/.test(arg)) {
        pyPrefix.push(arg);
        j++;
        continue;
      }
      break;
    }
    return null;
  }

  return null;
}

/**
 * Compact one-line summary for narrow table cells and bars.
 * Strips the `bash -c '...'` / `python3 -c '...'` wrapper and raw quote escapes,
 * returning a short language badge plus a single-line preview.
 */
export function commandSummary(argv: string): { badge: string | null; text: string } {
  const parsed = parseCommand(argv);
  if (parsed.kind === "script") {
    const lines = parsed.formatted
      .split("\n")
      .map((l) => l.trim())
      .filter((l) => l.length > 0 && !l.startsWith("#"));
    const preview = (lines.length > 0 ? lines.join(" ; ") : parsed.script.trim()).replace(/\s+/g, " ");
    return {
      badge: parsed.language === "python" ? "py" : parsed.interpreter,
      text: preview || parsed.raw,
    };
  }
  return { badge: null, text: argv.replace(/\s+/g, " ").trim() };
}

/**
 * Dedent uniform leading whitespace and trim outer blank lines.
 */
function normalizeBlock(source: string): string {
  const normalized = source.replace(/\r\n?/g, "\n");
  const rawLines = normalized.split("\n");
  let start = 0;
  let end = rawLines.length;
  while (start < end && rawLines[start]!.trim() === "") start++;
  while (end > start && rawLines[end - 1]!.trim() === "") end--;
  const lines = rawLines.slice(start, end);
  if (lines.length === 0) return "";

  let minIndent = Infinity;
  for (const line of lines) {
    if (line.trim() === "") continue;
    const match = line.match(/^[ \t]*/);
    const indent = match ? match[0].length : 0;
    if (indent < minIndent) minIndent = indent;
  }
  if (!Number.isFinite(minIndent) || minIndent === 0) {
    return lines.map((l) => l.replace(/[ \t]+$/, "")).join("\n");
  }
  return lines.map((l) => l.slice(minIndent).replace(/[ \t]+$/, "")).join("\n");
}

// ---------------------------------------------------------------------------
// Bash formatter
// ---------------------------------------------------------------------------

interface BashPiece {
  text: string;
  /** Separator that ended this piece: ";", "&&", "||", "|", "|&", or "" at end. */
  sep: string;
}

/**
 * Format a bash script for comfortable reading.
 *
 * - Restores clean newlines and dedents common leading whitespace.
 * - Leaves heredocs verbatim.
 * - Expands compressed single-line compound statements (`for ...; do ...; done`,
 *   `if ...; then ...; fi`) and `;`-separated statements onto indented lines.
 * - Splits long multi-stage `&&`, `||`, and `|` chains cleanly while keeping
 *   short guards like `... || exit 1` attached to their command.
 */
export function formatBash(source: string): string {
  const block = normalizeBlock(source);
  if (!block) return "";

  const inputLines = block.split("\n");
  const out: string[] = [];
  const pendingHeredocs: { delim: string; stripTabs: boolean }[] = [];

  for (let idx = 0; idx < inputLines.length; idx++) {
    const line = inputLines[idx]!;

    // Copy heredoc bodies verbatim until their closing delimiter.
    if (pendingHeredocs.length > 0) {
      out.push(line);
      const top = pendingHeredocs[0]!;
      const candidate = top.stripTabs ? line.replace(/^\t+/, "") : line;
      if (candidate === top.delim) {
        pendingHeredocs.shift();
      }
      continue;
    }

    const heredocs = findHeredocs(line);
    if (heredocs.length > 0) {
      pendingHeredocs.push(...heredocs);
      out.push(line);
      continue;
    }

    // Preserve blank lines, comments, explicit backslash continuations, and case arms.
    const trimmed = line.trim();
    if (trimmed === "" || trimmed.startsWith("#") || line.endsWith("\\") || hasCaseSyntax(trimmed)) {
      out.push(line);
      continue;
    }

    const baseIndent = line.match(/^[ \t]*/)?.[0] ?? "";
    const formattedLines = formatBashLine(trimmed, baseIndent);
    out.push(...formattedLines);
  }

  return out.join("\n");
}

function findHeredocs(line: string): { delim: string; stripTabs: boolean }[] {
  const found: { delim: string; stripTabs: boolean }[] = [];
  let inSingle = false;
  let inDouble = false;
  for (let i = 0; i < line.length; i++) {
    const ch = line[i]!;
    if (inSingle) {
      if (ch === "'") inSingle = false;
      continue;
    }
    if (inDouble) {
      if (ch === "\\" && i + 1 < line.length) {
        i++;
        continue;
      }
      if (ch === '"') inDouble = false;
      continue;
    }
    if (ch === "\\" && i + 1 < line.length) {
      i++;
      continue;
    }
    if (ch === "'") {
      inSingle = true;
      continue;
    }
    if (ch === '"') {
      inDouble = true;
      continue;
    }
    if (ch === "#" && (i === 0 || /[\s;|&(]/.test(line[i - 1]!))) break;
    if (ch === "<" && line[i + 1] === "<" && line[i + 2] !== "<") {
      let j = i + 2;
      let stripTabs = false;
      if (line[j] === "-") {
        stripTabs = true;
        j++;
      }
      while (j < line.length && (line[j] === " " || line[j] === "\t")) j++;
      if (j >= line.length) break;
      let delim = "";
      if (line[j] === "'" || line[j] === '"') {
        const q = line[j]!;
        const end = line.indexOf(q, j + 1);
        if (end > j + 1) {
          delim = line.slice(j + 1, end);
          i = end;
        }
      } else {
        const start = j;
        while (j < line.length && !/[\s;|&<>()]/.test(line[j]!)) {
          if (line[j] === "\\" && j + 1 < line.length) {
            delim += line[j + 1]!;
            j += 2;
          } else {
            delim += line[j]!;
            j++;
          }
        }
        if (j > start) i = j - 1;
      }
      if (delim) found.push({ delim, stripTabs });
    }
  }
  return found;
}

function hasCaseSyntax(trimmed: string): boolean {
  return /^(case\b|esac\b)/.test(trimmed) || /(^|\s);;(&)?(\s|$)/.test(trimmed);
}

/**
 * Split one bash line at top-level `;`, `&&`, `||`, `|`, `|&` (outside quotes,
 * parens, `[[...]]`, `${...}`, backticks, comments, and `\;` escapes).
 */
function splitBashTopLevel(line: string): { pieces: BashPiece[]; comment: string } | null {
  const pieces: BashPiece[] = [];
  let start = 0;
  let inSingle = false;
  let inDouble = false;
  let inBacktick = false;
  let parenDepth = 0;
  let braceDepth = 0;
  let dblBracketDepth = 0;
  let comment = "";

  for (let i = 0; i < line.length; i++) {
    const ch = line[i]!;
    if (inSingle) {
      if (ch === "'") inSingle = false;
      continue;
    }
    if (inDouble) {
      if (ch === "\\" && i + 1 < line.length) {
        i++;
        continue;
      }
      if (ch === '"') inDouble = false;
      continue;
    }
    if (inBacktick) {
      if (ch === "\\" && i + 1 < line.length) {
        i++;
        continue;
      }
      if (ch === "`") inBacktick = false;
      continue;
    }
    if (ch === "\\" && i + 1 < line.length) {
      i++;
      continue;
    }
    if (ch === "$" && line[i + 1] === "'") {
      // ANSI-C string $'...'
      i += 2;
      while (i < line.length) {
        if (line[i] === "\\" && i + 1 < line.length) {
          i += 2;
          continue;
        }
        if (line[i] === "'") break;
        i++;
      }
      continue;
    }
    if (ch === "'") {
      inSingle = true;
      continue;
    }
    if (ch === '"') {
      inDouble = true;
      continue;
    }
    if (ch === "`") {
      inBacktick = true;
      continue;
    }
    if (ch === "#" && (i === 0 || /[\s;|&({]/.test(line[i - 1]!))) {
      comment = line.slice(i);
      const tail = line.slice(start, i).trim();
      if (tail) pieces.push({ text: tail, sep: "" });
      return { pieces, comment };
    }
    if (ch === "$" && line[i + 1] === "{") {
      braceDepth++;
      i++;
      continue;
    }
    if (ch === "}" && braceDepth > 0) {
      braceDepth--;
      continue;
    }
    if (ch === "(") {
      parenDepth++;
      continue;
    }
    if (ch === ")" && parenDepth > 0) {
      parenDepth--;
      continue;
    }
    if (ch === "[" && line[i + 1] === "[" && (i === 0 || /[\s;|&(]/.test(line[i - 1]!))) {
      dblBracketDepth++;
      i++;
      continue;
    }
    if (ch === "]" && line[i + 1] === "]" && dblBracketDepth > 0) {
      dblBracketDepth--;
      i++;
      continue;
    }

    if (parenDepth === 0 && braceDepth === 0 && dblBracketDepth === 0) {
      const two = line.slice(i, i + 2);
      if (two === "&&" || two === "||" || two === "|&") {
        pieces.push({ text: line.slice(start, i).trim(), sep: two });
        i++;
        start = i + 1;
        continue;
      }
      if (ch === ";") {
        if (line[i + 1] === ";" || line[i + 1] === "&") return null; // case terminator
        pieces.push({ text: line.slice(start, i).trim(), sep: ";" });
        start = i + 1;
        continue;
      }
      if (ch === "|") {
        pieces.push({ text: line.slice(start, i).trim(), sep: "|" });
        start = i + 1;
        continue;
      }
    }
  }

  if (inSingle || inDouble || inBacktick || parenDepth > 0 || braceDepth > 0 || dblBracketDepth > 0) {
    return null;
  }

  const tail = line.slice(start).trim();
  if (tail) pieces.push({ text: tail, sep: "" });
  return { pieces, comment };
}

const SHORT_GUARD_RE = /^(exit(\s+\d+)?|return(\s+\d+)?|true|false|break|continue)$/;

function formatBashLine(trimmed: string, baseIndent: string): string[] {
  const split = splitBashTopLevel(trimmed);
  if (!split || split.pieces.length <= 1) {
    return [baseIndent + trimmed];
  }

  const { pieces, comment } = split;
  const hasSemi = pieces.some((p) => p.sep === ";");
  const chainOps = pieces.filter((p) => p.sep === "&&" || p.sep === "||" || p.sep === "|" || p.sep === "|&");

  // Keep short lines without semicolons on one line, and keep a single command
  // with a short guard (`cargo clippy ... || exit 1`) on one line.
  if (!hasSemi) {
    if (trimmed.length <= 90 && chainOps.length <= 1) {
      return [baseIndent + trimmed];
    }
    if (
      pieces.length === 2 &&
      pieces[0]!.sep === "||" &&
      SHORT_GUARD_RE.test(pieces[1]!.text)
    ) {
      return [baseIndent + trimmed];
    }
  }

  // Group `;`-separated statements, keeping `if ...; then` / `for ...; do` headers together.
  const statements: BashPiece[][] = [];
  let curStmt: BashPiece[] = [];
  for (const piece of pieces) {
    curStmt.push(piece);
    if (piece.sep === ";") {
      statements.push(curStmt);
      curStmt = [];
    }
  }
  if (curStmt.length > 0) statements.push(curStmt);

  // Merge `if/elif/for/while/until ...;` followed by `then`/`do` into one header line,
  // and split any command immediately after `then`/`do`/`else` onto its own line.
  interface StmtLine {
    pieces: BashPiece[];
    endsWithBlockOpen: boolean;
    dedentBefore: boolean;
    indentAfter: boolean;
  }
  const expanded: StmtLine[] = [];

  const pushPieces = (ps: BashPiece[], forcedOpen = false) => {
    if (ps.length === 0) return;
    const first = ps[0]!.text;
    const firstWord = first.split(/\s+/)[0] ?? "";

    // Split leading `then <cmd>`, `do <cmd>`, `else <cmd>` when compressed onto one line.
    if ((firstWord === "then" || firstWord === "do" || firstWord === "else") && first.length > firstWord.length) {
      const rest = first.slice(firstWord.length).trim();
      const restPieces: BashPiece[] = [{ text: rest, sep: ps[0]!.sep }, ...ps.slice(1)];
      if ((firstWord === "then" || firstWord === "do") && expanded.length > 0) {
        const prev = expanded[expanded.length - 1]!;
        const prevText = prev.pieces[0]?.text ?? "";
        if (/^(if|elif|for|while|until)\b/.test(prevText) && !prev.endsWithBlockOpen) {
          prev.pieces[prev.pieces.length - 1]!.text += `; ${firstWord}`;
          prev.endsWithBlockOpen = true;
          prev.indentAfter = true;
          pushPieces(restPieces);
          return;
        }
      }
      expanded.push({
        pieces: [{ text: firstWord, sep: "" }],
        endsWithBlockOpen: true,
        dedentBefore: firstWord === "else",
        indentAfter: true,
      });
      pushPieces(restPieces);
      return;
    }

    if ((firstWord === "then" || firstWord === "do") && ps.length === 1 && expanded.length > 0) {
      const prev = expanded[expanded.length - 1]!;
      const prevText = prev.pieces[0]?.text ?? "";
      if (/^(if|elif|for|while|until)\b/.test(prevText) && !prev.endsWithBlockOpen) {
        prev.pieces[prev.pieces.length - 1]!.text += `; ${firstWord}`;
        prev.endsWithBlockOpen = true;
        prev.indentAfter = true;
        return;
      }
    }

    const dedentBefore = firstWord === "fi" || firstWord === "done" || firstWord === "else" || firstWord === "elif" || firstWord === "}";
    const indentAfter = forcedOpen || firstWord === "then" || firstWord === "do" || firstWord === "else" || first.endsWith("{");
    expanded.push({ pieces: ps, endsWithBlockOpen: indentAfter, dedentBefore, indentAfter });
  };

  for (const stmt of statements) {
    // Strip trailing `;` separator since each statement gets its own line.
    const cleaned = stmt.map((p, k) => (k === stmt.length - 1 && p.sep === ";" ? { ...p, sep: "" } : p));
    pushPieces(cleaned);
  }

  const out: string[] = [];
  let depth = 0;

  for (let sIdx = 0; sIdx < expanded.length; sIdx++) {
    const stmt = expanded[sIdx]!;
    if (stmt.dedentBefore && depth > 0) depth--;
    const indent = baseIndent + "  ".repeat(depth);
    const isLast = sIdx === expanded.length - 1;
    const tailComment = isLast && comment ? ` ${comment}` : "";

    const rendered = renderBashChain(stmt.pieces, indent);
    if (rendered.length > 0 && tailComment) {
      rendered[rendered.length - 1] += tailComment;
    }
    out.push(...rendered);

    if (stmt.indentAfter) depth++;
  }

  return out;
}

function renderBashChain(pieces: BashPiece[], indent: string): string[] {
  if (pieces.length === 0) return [];
  const joined = pieces.map((p) => (p.sep ? `${p.text} ${p.sep}` : p.text)).join(" ");
  if (pieces.length === 1 || (joined.length <= 90 && pieces.length <= 2)) {
    return [indent + joined];
  }
  if (pieces.length === 2 && pieces[0]!.sep === "||" && SHORT_GUARD_RE.test(pieces[1]!.text)) {
    return [indent + joined];
  }

  // Merge a trailing short guard (`|| exit 1`) onto the preceding segment.
  const merged: BashPiece[] = [];
  for (let i = 0; i < pieces.length; i++) {
    const cur = pieces[i]!;
    const next = pieces[i + 1];
    if (next && cur.sep === "||" && SHORT_GUARD_RE.test(next.text) && !next.sep) {
      merged.push({ text: `${cur.text} || ${next.text}`, sep: "" });
      i++;
    } else {
      merged.push(cur);
    }
  }
  if (merged.length === 1) {
    return [indent + merged[0]!.text];
  }

  const contIndent = indent + "  ";
  return merged.map((p, idx) => {
    const pad = idx === 0 ? indent : contIndent;
    return pad + (p.sep ? `${p.text} ${p.sep}` : p.text);
  });
}

// ---------------------------------------------------------------------------
// Python formatter
// ---------------------------------------------------------------------------

const PY_COMPOUND_RE = /^(async\s+(def|for|with)|if|elif|else|for|while|with|def|class|try|except|finally)\b/;

/**
 * Format a Python script for reading.
 *
 * - Multi-line scripts are dedented and trimmed without altering indentation or
 *   line breaks (Python is indentation-sensitive).
 * - Single-line scripts (`python3 -c 'import json, sys; ...'`) have top-level
 *   `;`-separated statements and simple compound headers (`for x in xs: ...`)
 *   expanded onto properly indented lines.
 */
export function formatPython(source: string): string {
  const block = normalizeBlock(source);
  if (!block) return "";
  if (block.includes("\n")) return block;

  return formatPythonOneLiner(block, 0).join("\n");
}

function formatPythonOneLiner(line: string, indentLevel: number): string[] {
  const trimmed = line.trim();
  if (!trimmed) return [];
  const indent = "    ".repeat(indentLevel);

  // If the line starts with a compound header (e.g. `for x in xs: a(); b()`),
  // split at the depth-0 `:` (not `:=`) and format the inline suite indented.
  if (PY_COMPOUND_RE.test(trimmed)) {
    const colonIdx = findPythonTopLevelColon(trimmed);
    if (colonIdx >= 0) {
      const header = trimmed.slice(0, colonIdx + 1).trimEnd();
      const body = trimmed.slice(colonIdx + 1).trim();
      if (body && !body.startsWith("#")) {
        const bodyLines = formatPythonOneLiner(body, indentLevel + 1);
        return [indent + header, ...bodyLines];
      }
      return [indent + trimmed];
    }
  }

  // Otherwise split at top-level `;` outside strings, comments, and brackets.
  const parts = splitPythonTopLevelSemi(trimmed);
  if (parts.length <= 1) {
    return [indent + trimmed];
  }
  const out: string[] = [];
  for (const part of parts) {
    out.push(...formatPythonOneLiner(part, indentLevel));
  }
  return out;
}

/** Advance past a Python string literal starting at `i` (at quote char). Returns index after closing quote. */
function scanPythonString(s: string, quoteStart: number): number {
  const q = s[quoteStart]!;
  const triple = s[quoteStart + 1] === q && s[quoteStart + 2] === q;
  let i = quoteStart + (triple ? 3 : 1);
  while (i < s.length) {
    const ch = s[i]!;
    if (ch === "\\" && i + 1 < s.length) {
      i += 2;
      continue;
    }
    if (triple) {
      if (ch === q && s[i + 1] === q && s[i + 2] === q) return i + 3;
    } else if (ch === q) {
      return i + 1;
    }
    i++;
  }
  return s.length;
}

function findPythonTopLevelColon(s: string): number {
  let depth = 0;
  let i = 0;
  while (i < s.length) {
    const ch = s[i]!;
    if (ch === "'" || ch === '"') {
      i = scanPythonString(s, i);
      continue;
    }
    if (ch === "#") break;
    if (ch === "(" || ch === "[" || ch === "{") {
      depth++;
      i++;
      continue;
    }
    if (ch === ")" || ch === "]" || ch === "}") {
      if (depth > 0) depth--;
      i++;
      continue;
    }
    if (depth === 0) {
      if (ch === ":" && s[i + 1] === "=") {
        i += 2; // walrus operator :=
        continue;
      }
      if (ch === ":") return i;
    }
    i++;
  }
  return -1;
}

function splitPythonTopLevelSemi(s: string): string[] {
  const parts: string[] = [];
  let start = 0;
  let depth = 0;
  let i = 0;
  while (i < s.length) {
    const ch = s[i]!;
    if (ch === "'" || ch === '"') {
      i = scanPythonString(s, i);
      continue;
    }
    if (ch === "#") break;
    if (ch === "(" || ch === "[" || ch === "{") {
      depth++;
      i++;
      continue;
    }
    if (ch === ")" || ch === "]" || ch === "}") {
      if (depth > 0) depth--;
      i++;
      continue;
    }
    if (depth === 0 && ch === ";") {
      const piece = s.slice(start, i).trim();
      if (piece) parts.push(piece);
      start = i + 1;
      i++;
      continue;
    }
    i++;
  }
  const tail = s.slice(start).trim();
  if (tail) parts.push(tail);
  return parts;
}

// ---------------------------------------------------------------------------
// Long plain command formatter (non-`-c` commands)
// ---------------------------------------------------------------------------

function formatPlainCommand(words: string[] | null, raw: string): string {
  const normalized = raw.replace(/\r\n?/g, "\n").trim();
  if (!normalized) return "";
  if (normalized.includes("\n")) return normalized;
  if (!words || words.length < 6 || normalized.length <= 100) return normalized;

  // Group flag + value pairs (e.g. `-p local_cell`, `--test-threads 4`) so a
  // long command wraps cleanly with `\` continuations without splitting a flag
  // from its argument.
  const quoted = words.map(shellQuoteWord);
  const groups: string[] = [];
  let i = 0;
  // Keep leading executable + subcommand words together (up to first flag or 2 words).
  const head: string[] = [quoted[0]!];
  i = 1;
  while (i < words.length && i < 3 && !words[i]!.startsWith("-")) {
    head.push(quoted[i]!);
    i++;
  }
  groups.push(head.join(" "));

  while (i < words.length) {
    const rawW = words[i]!;
    const qW = quoted[i]!;
    if (rawW === "--") {
      groups.push("--");
      i++;
      continue;
    }
    // Pair `-p foo` or `--features bar` when the next token is not a flag.
    if (
      rawW.startsWith("-") &&
      !rawW.includes("=") &&
      i + 1 < words.length &&
      !words[i + 1]!.startsWith("-")
    ) {
      groups.push(`${qW} ${quoted[i + 1]!}`);
      i += 2;
      continue;
    }
    groups.push(qW);
    i++;
  }

  // Pack groups onto lines up to ~84 columns with `\` continuations.
  const lines: string[] = [];
  let cur = groups[0] ?? "";
  for (let g = 1; g < groups.length; g++) {
    const next = groups[g]!;
    const indent = lines.length === 0 ? "" : "  ";
    if (cur.length + 1 + next.length <= 84 && next !== "--") {
      cur += " " + next;
    } else {
      lines.push(indent + cur + " \\");
      cur = next;
    }
  }
  lines.push((lines.length === 0 ? "" : "  ") + cur);
  return lines.join("\n");
}
