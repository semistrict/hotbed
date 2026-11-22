// Lightweight, lossless syntax tokenizers for bash and python3.
//
// Invariant: for every input string `code`,
//   tokenize(code).map((line) => line.map((t) => t.text).join("")).join("\n") === code
// so highlighting never drops, alters, or reorders a single character.

import type { ScriptLanguage } from "./command";

export type TokenKind =
  | "plain"
  | "keyword"
  | "builtin"
  | "string"
  | "comment"
  | "variable"
  | "number"
  | "operator"
  | "flag"
  | "command"
  | "function"
  | "decorator";

export interface Token {
  kind: TokenKind;
  text: string;
}

export function tokenize(code: string, language: ScriptLanguage): Token[][] {
  return language === "python" ? tokenizePython(code) : tokenizeBash(code);
}

/** Push text onto the per-line token output, splitting on any embedded newlines. */
function pushMultiLine(lines: Token[][], kind: TokenKind, text: string) {
  const parts = text.split("\n");
  for (let i = 0; i < parts.length; i++) {
    if (i > 0) lines.push([]);
    const part = parts[i]!;
    if (part.length > 0) {
      const cur = lines[lines.length - 1]!;
      const prev = cur[cur.length - 1];
      if (prev && prev.kind === kind) prev.text += part;
      else cur.push({ kind, text: part });
    }
  }
}

// ---------------------------------------------------------------------------
// Bash tokenizer
// ---------------------------------------------------------------------------

const BASH_KEYWORDS = new Set([
  "if", "then", "elif", "else", "fi",
  "for", "in", "while", "until", "do", "done",
  "case", "esac", "select", "function", "time", "coproc",
]);

const BASH_BUILTINS = new Set([
  "set", "unset", "export", "local", "readonly", "declare", "typeset",
  "echo", "printf", "read", "exit", "return", "break", "continue",
  "cd", "pwd", "pushd", "popd", "shift", "eval", "exec", "source",
  "trap", "wait", "true", "false", "test", "env", "ulimit", "umask",
]);

export function tokenizeBash(code: string): Token[][] {
  const lines: Token[][] = [[]];
  const n = code.length;
  let i = 0;
  let atCmdPos = true;
  let pendingHeredocs: { delim: string; stripTabs: boolean }[] = [];

  const push = (kind: TokenKind, text: string) => pushMultiLine(lines, kind, text);

  while (i < n) {
    const ch = code[i]!;

    // Newline: check if a heredoc body starts on the next line.
    if (ch === "\n") {
      push("plain", "\n");
      i++;
      atCmdPos = true;
      while (pendingHeredocs.length > 0 && i < n) {
        const hd = pendingHeredocs[0]!;
        const nl = code.indexOf("\n", i);
        const end = nl < 0 ? n : nl;
        const bodyLine = code.slice(i, end);
        const candidate = hd.stripTabs ? bodyLine.replace(/^\t+/, "") : bodyLine;
        if (candidate === hd.delim) {
          push("keyword", bodyLine);
          pendingHeredocs.shift();
        } else {
          push("string", bodyLine);
        }
        i = end;
        if (i < n && code[i] === "\n") {
          push("plain", "\n");
          i++;
        }
      }
      continue;
    }

    // Horizontal whitespace
    if (ch === " " || ch === "\t" || ch === "\r") {
      const start = i;
      while (i < n && (code[i] === " " || code[i] === "\t" || code[i] === "\r")) i++;
      push("plain", code.slice(start, i));
      continue;
    }

    // Line continuation `\` at end of line
    if (ch === "\\" && code[i + 1] === "\n") {
      push("operator", "\\");
      push("plain", "\n");
      i += 2;
      continue;
    }

    // Comment `#...` at word start
    if (ch === "#" && (i === 0 || /[\s;|&({]/.test(code[i - 1]!))) {
      const nl = code.indexOf("\n", i);
      const end = nl < 0 ? n : nl;
      push("comment", code.slice(i, end));
      i = end;
      continue;
    }

    // ANSI-C string $'...'
    if (ch === "$" && code[i + 1] === "'") {
      const start = i;
      i += 2;
      while (i < n) {
        if (code[i] === "\\" && i + 1 < n) {
          i += 2;
          continue;
        }
        if (code[i] === "'") {
          i++;
          break;
        }
        i++;
      }
      push("string", code.slice(start, i));
      atCmdPos = false;
      continue;
    }

    // Single-quoted string '...'
    if (ch === "'") {
      const start = i;
      const close = code.indexOf("'", i + 1);
      i = close < 0 ? n : close + 1;
      push("string", code.slice(start, i));
      atCmdPos = false;
      continue;
    }

    // Double-quoted string "..." (highlight embedded $VAR / ${VAR} inside)
    if (ch === '"') {
      push("string", '"');
      i++;
      let segStart = i;
      while (i < n) {
        const c = code[i]!;
        if (c === "\\" && i + 1 < n) {
          i += 2;
          continue;
        }
        if (c === '"') {
          if (i > segStart) push("string", code.slice(segStart, i));
          push("string", '"');
          i++;
          segStart = i;
          break;
        }
        if (c === "$") {
          const varEnd = scanBashVarEnd(code, i);
          if (varEnd > i + 1) {
            if (i > segStart) push("string", code.slice(segStart, i));
            push("variable", code.slice(i, varEnd));
            i = varEnd;
            segStart = i;
            continue;
          }
        }
        i++;
      }
      if (i > segStart) push("string", code.slice(segStart, i));
      atCmdPos = false;
      continue;
    }

    // Variable expansion $VAR / ${...} / $? / $@ / $1
    if (ch === "$") {
      const varEnd = scanBashVarEnd(code, i);
      if (varEnd > i + 1) {
        push("variable", code.slice(i, varEnd));
        i = varEnd;
        atCmdPos = false;
        continue;
      }
    }

    // Heredoc operator << or <<-
    if (ch === "<" && code[i + 1] === "<" && code[i + 2] !== "<") {
      const opEnd = code[i + 2] === "-" ? i + 3 : i + 2;
      const stripTabs = code[i + 2] === "-";
      push("operator", code.slice(i, opEnd));
      i = opEnd;
      // Scan delimiter word
      const wsStart = i;
      while (i < n && (code[i] === " " || code[i] === "\t")) i++;
      if (i > wsStart) push("plain", code.slice(wsStart, i));
      if (i < n && code[i] !== "\n") {
        let delim = "";
        const dStart = i;
        if (code[i] === "'" || code[i] === '"') {
          const q = code[i]!;
          const end = code.indexOf(q, i + 1);
          i = end < 0 ? n : end + 1;
          delim = code.slice(dStart + 1, end < 0 ? n : end);
          push("string", code.slice(dStart, i));
        } else {
          while (i < n && !/[\s;|&<>()]/.test(code[i]!)) {
            if (code[i] === "\\" && i + 1 < n) {
              delim += code[i + 1]!;
              i += 2;
            } else {
              delim += code[i]!;
              i++;
            }
          }
          push("keyword", code.slice(dStart, i));
        }
        if (delim) pendingHeredocs.push({ delim, stripTabs });
      }
      continue;
    }

    // File descriptor redirection like 2>&1 or 2>
    if (/[0-9]/.test(ch) && (code[i + 1] === ">" || code[i + 1] === "<")) {
      const m = code.slice(i).match(/^[0-9]+(>>|>&[0-9-]+|>&|<>|>|<)/);
      if (m) {
        push("operator", m[0]);
        i += m[0].length;
        continue;
      }
    }

    // Multi-char operators
    const two = code.slice(i, i + 2);
    if (two === "&&" || two === "||" || two === "|&" || two === ";;" || two === ";&" || two === ">>" || two === ">&" || two === "<&" || two === "<<<".slice(0, 2)) {
      if (code.slice(i, i + 3) === "<<<" || code.slice(i, i + 3) === ";;&") {
        push("operator", code.slice(i, i + 3));
        i += 3;
        if (two === ";;") atCmdPos = true;
        continue;
      }
      if (two !== "<<") {
        push("operator", two);
        i += 2;
        if (two === "&&" || two === "||" || two === "|&" || two === ";;" || two === ";&") atCmdPos = true;
        continue;
      }
    }

    // Single-char operators & punctuation
    if (ch === "|" || ch === ";" || ch === "&" || ch === ">" || ch === "<" || ch === "(" || ch === ")" || ch === "{" || ch === "}") {
      push("operator", ch);
      i++;
      if (ch === "|" || ch === ";" || ch === "&" || ch === "(" || ch === "{") atCmdPos = true;
      continue;
    }

    // Word token
    const start = i;
    while (i < n) {
      const c = code[i]!;
      if (c === "\\" && i + 1 < n && code[i + 1] !== "\n") {
        i += 2;
        continue;
      }
      if (/[\s'"$|;&<>(){}]/.test(c)) break;
      i++;
    }
    if (i === start) {
      push("plain", ch);
      i++;
      continue;
    }
    const word = code.slice(start, i);

    // Environment / shell variable assignment: NAME=... or NAME+=(...)
    const assignMatch = word.match(/^([A-Za-z_][A-Za-z0-9_]*)(\+?=)(.*)$/);
    if (assignMatch && (atCmdPos || isAfterExportOrEnv(lines[lines.length - 1]!))) {
      const [, name, eq, val] = assignMatch;
      push("variable", name!);
      push("operator", eq!);
      if (val) push(isNumber(val) ? "number" : "plain", val);
      // Assignments at command position leave command position open (`FOO=1 cmd`).
      continue;
    }

    if (atCmdPos) {
      if (BASH_KEYWORDS.has(word)) {
        push("keyword", word);
        // After `if`, `then`, `elif`, `else`, `do`, `time`, `!`, next word is still a command.
        atCmdPos = word === "if" || word === "then" || word === "elif" || word === "else" || word === "do" || word === "while" || word === "until" || word === "time";
      } else if (BASH_BUILTINS.has(word)) {
        push("builtin", word);
        atCmdPos = word === "env" || word === "exec";
      } else if (word === "!" || word === "[[" || word === "]") {
        push("operator", word);
      } else {
        push("command", word);
        atCmdPos = false;
      }
    } else if (BASH_KEYWORDS.has(word) && (word === "in" || word === "do" || word === "then" || word === "done" || word === "fi")) {
      push("keyword", word);
      if (word === "do" || word === "then") atCmdPos = true;
    } else if (word === "--" || /^-[A-Za-z0-9]/.test(word) || /^--[A-Za-z0-9]/.test(word)) {
      // Split `--flag=value` so the flag and value read distinctly.
      const eq = word.indexOf("=");
      if (eq > 0) {
        push("flag", word.slice(0, eq));
        push("operator", "=");
        const val = word.slice(eq + 1);
        if (val) push(isNumber(val) ? "number" : "plain", val);
      } else {
        push("flag", word);
      }
    } else if (isNumber(word)) {
      push("number", word);
    } else {
      push("plain", word);
    }
  }

  return lines;
}

function scanBashVarEnd(code: string, start: number): number {
  const n = code.length;
  const next = code[start + 1];
  if (!next) return start + 1;
  if (next === "{") {
    let depth = 1;
    let i = start + 2;
    while (i < n && depth > 0) {
      if (code[i] === "\\" && i + 1 < n) {
        i += 2;
        continue;
      }
      if (code[i] === "{") depth++;
      else if (code[i] === "}") depth--;
      i++;
    }
    return i;
  }
  if (/[A-Za-z_]/.test(next)) {
    let i = start + 2;
    while (i < n && /[A-Za-z0-9_]/.test(code[i]!)) i++;
    return i;
  }
  if (/[0-9?*@$#!-]/.test(next)) {
    return start + 2;
  }
  return start + 1;
}

function isAfterExportOrEnv(lineTokens: Token[]): boolean {
  for (let i = lineTokens.length - 1; i >= 0; i--) {
    const t = lineTokens[i]!;
    if (t.kind === "plain" && t.text.trim() === "") continue;
    if (t.kind === "builtin" && (t.text === "export" || t.text === "local" || t.text === "readonly" || t.text === "declare" || t.text === "env")) {
      return true;
    }
    if (t.kind === "command") return false;
    if (t.kind === "operator" && (t.text === ";" || t.text === "&&" || t.text === "||" || t.text === "|")) {
      return false;
    }
  }
  return false;
}

function isNumber(s: string): boolean {
  return /^-?\d+(\.\d+)?$/.test(s);
}

// ---------------------------------------------------------------------------
// Python tokenizer
// ---------------------------------------------------------------------------

const PY_KEYWORDS = new Set([
  "and", "as", "assert", "async", "await", "break", "case", "class",
  "continue", "def", "del", "elif", "else", "except", "finally", "for",
  "from", "global", "if", "import", "in", "is", "lambda", "match",
  "nonlocal", "not", "or", "pass", "raise", "return", "try", "while",
  "with", "yield",
]);

const PY_CONSTANTS = new Set(["True", "False", "None", "self", "cls"]);

const PY_BUILTINS = new Set([
  "abs", "all", "any", "bin", "bool", "bytes", "callable", "chr", "dict",
  "dir", "divmod", "enumerate", "eval", "exec", "filter", "float", "format",
  "frozenset", "getattr", "hasattr", "hash", "hex", "id", "input", "int",
  "isinstance", "issubclass", "iter", "len", "list", "map", "max", "min",
  "next", "object", "open", "ord", "pow", "print", "range", "repr",
  "reversed", "round", "set", "setattr", "slice", "sorted", "str", "sum",
  "super", "tuple", "type", "vars", "zip",
  "Exception", "ValueError", "TypeError", "KeyError", "RuntimeError",
  "OSError", "FileNotFoundError", "AssertionError", "SystemExit",
]);

export function tokenizePython(code: string): Token[][] {
  const lines: Token[][] = [[]];
  const n = code.length;
  let i = 0;
  let prevKeyword = "";

  const push = (kind: TokenKind, text: string) => pushMultiLine(lines, kind, text);

  while (i < n) {
    const ch = code[i]!;

    // Whitespace / newlines
    if (ch === "\n") {
      push("plain", "\n");
      i++;
      prevKeyword = "";
      continue;
    }
    if (ch === " " || ch === "\t" || ch === "\r") {
      const start = i;
      while (i < n && (code[i] === " " || code[i] === "\t" || code[i] === "\r")) i++;
      push("plain", code.slice(start, i));
      continue;
    }

    // Comment `#...`
    if (ch === "#") {
      const nl = code.indexOf("\n", i);
      const end = nl < 0 ? n : nl;
      push("comment", code.slice(i, end));
      i = end;
      continue;
    }

    // Decorator `@name` at start of line
    if (ch === "@" && isLineLeading(lines[lines.length - 1]!)) {
      const start = i;
      i++;
      while (i < n && /[A-Za-z0-9_.]/.test(code[i]!)) i++;
      push("decorator", code.slice(start, i));
      continue;
    }

    // String literal (with optional r/f/b/u prefix)
    const strPrefix = matchPyStringPrefix(code, i);
    if (strPrefix !== null) {
      const start = i;
      const qStart = i + strPrefix.length;
      const q = code[qStart]!;
      const triple = code[qStart + 1] === q && code[qStart + 2] === q;
      i = qStart + (triple ? 3 : 1);
      while (i < n) {
        const c = code[i]!;
        if (c === "\\" && i + 1 < n) {
          i += 2;
          continue;
        }
        if (triple) {
          if (c === q && code[i + 1] === q && code[i + 2] === q) {
            i += 3;
            break;
          }
        } else if (c === q || c === "\n") {
          if (c === q) i++;
          break;
        }
        i++;
      }
      push("string", code.slice(start, i));
      prevKeyword = "";
      continue;
    }

    // Number literal (hex, bin, oct, int, float, scientific)
    if (/[0-9]/.test(ch) || (ch === "." && i + 1 < n && /[0-9]/.test(code[i + 1]!))) {
      const m = code
        .slice(i)
        .match(/^(?:0[xX][0-9a-fA-F_]+|0[bB][01_]+|0[oO][0-7_]+|\d[\d_]*(?:\.[\d_]*)?(?:[eE][+-]?[\d_]+)?|\.\d[\d_]*(?:[eE][+-]?[\d_]+)?)[jJ]?/);
      if (m) {
        push("number", m[0]);
        i += m[0].length;
        prevKeyword = "";
        continue;
      }
    }

    // Identifier / keyword / builtin / function name
    if (/[A-Za-z_]/.test(ch)) {
      const start = i;
      while (i < n && /[A-Za-z0-9_]/.test(code[i]!)) i++;
      const word = code.slice(start, i);
      if (prevKeyword === "def" || prevKeyword === "class") {
        push("function", word);
        prevKeyword = "";
      } else if (PY_KEYWORDS.has(word)) {
        push("keyword", word);
        prevKeyword = word === "def" || word === "class" ? word : "";
      } else if (PY_CONSTANTS.has(word)) {
        push("variable", word);
        prevKeyword = "";
      } else if (PY_BUILTINS.has(word)) {
        push("builtin", word);
        prevKeyword = "";
      } else if (code[i] === "(") {
        push("function", word);
        prevKeyword = "";
      } else {
        push("plain", word);
        prevKeyword = "";
      }
      continue;
    }

    // Multi-char operators
    const three = code.slice(i, i + 3);
    if (three === "**=" || three === "//=" || three === "<<=" || three === ">>=") {
      push("operator", three);
      i += 3;
      prevKeyword = "";
      continue;
    }
    const two = code.slice(i, i + 2);
    if (
      two === "==" || two === "!=" || two === "<=" || two === ">=" ||
      two === "->" || two === ":=" || two === "**" || two === "//" ||
      two === "<<" || two === ">>" || two === "+=" || two === "-=" ||
      two === "*=" || two === "/=" || two === "%=" || two === "&=" ||
      two === "|=" || two === "^="
    ) {
      push("operator", two);
      i += 2;
      prevKeyword = "";
      continue;
    }

    if (/[+\-*/%=<>!&|^~@:]/.test(ch)) {
      push("operator", ch);
      i++;
      prevKeyword = "";
      continue;
    }

    push("plain", ch);
    i++;
    prevKeyword = "";
  }

  return lines;
}

function matchPyStringPrefix(code: string, i: number): string | null {
  const ch = code[i]!;
  if (ch === "'" || ch === '"') return "";
  const m = code.slice(i, i + 3).match(/^([rRbBuUfF]{1,2})['"]/);
  return m ? m[1]! : null;
}

function isLineLeading(lineTokens: Token[]): boolean {
  return lineTokens.every((t) => t.kind === "plain" && t.text.trim() === "");
}
