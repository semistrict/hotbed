// Bash source analysis for display: find scripts embedded in a shell script
// (`python3 -c '…'`, `bash -c '…'`, heredocs fed to an interpreter) so each is
// highlighted in its own language, and lay out one-line scripts readably.
// Adapted from T3 Code's embeddedScripts (MIT, github.com/pingdotgg/t3code).

import { forEachChild, parse, type Command, type DoubleQuotedChild, type HereDoc, type ParsedScript, type SyntaxNode, type Word, type WordPart } from "unbash";
import { print } from "unbash/printer";
import type { ScriptLanguage } from "./command";

/** A script inside a shell script; starts[i]..ends[i] spans the source spelling of text[i], escapes included. */
export interface EmbeddedScript {
  readonly language: ScriptLanguage;
  readonly text: string;
  readonly starts: readonly number[];
  readonly ends: readonly number[];
}

interface MappedText {
  readonly text: string;
  readonly starts: readonly number[];
  readonly ends: readonly number[];
}

interface Interpreter {
  readonly language: ScriptLanguage;
  readonly inline: (flag: string) => boolean;
  readonly valueOptions: ReadonlySet<string>;
  /** Options after which the script comes from a file or module, not inline or stdin. */
  readonly scriptFileOptions: ReadonlySet<string>;
}

// bash -c 'python3 -c "…"' nests; past this depth the rest stays shell-highlighted.
const MAX_DEPTH = 4;

const SHELL: Interpreter = {
  language: "bash",
  inline: (flag) => /^-[a-zA-Z]*c[a-zA-Z]*$/.test(flag),
  valueOptions: new Set(["-o", "-O", "+o", "+O", "--rcfile", "--init-file"]),
  scriptFileOptions: new Set(),
};

const PYTHON: Interpreter = {
  language: "python",
  inline: (flag) => flag === "-c",
  valueOptions: new Set(["-W", "-X", "--check-hash-based-pycs"]),
  scriptFileOptions: new Set(["-m"]),
};

const SHELL_NAMES = new Set(["sh", "bash", "zsh", "dash", "ash", "ksh", "mksh"]);
const PYTHON_RE = /^(python(\d+(\.\d+)*)?|pypy3?)$/;

function interpreterFor(name: string): Interpreter | undefined {
  if (SHELL_NAMES.has(name)) return SHELL;
  if (PYTHON_RE.test(name)) return PYTHON;
  return undefined;
}

interface Wrapper {
  /** Options that take a value. */
  readonly valueOptions: ReadonlySet<string>;
  /** Arguments between the options and the command, such as timeout's duration. */
  readonly leading?: number;
}

// Programs that run a command given after their own arguments.
const PROGRAM_WRAPPERS = new Map<string, Wrapper>([
  ["env", { valueOptions: new Set(["-u", "--unset", "-C", "--chdir"]) }],
  ["sudo", { valueOptions: new Set(["-u", "--user", "-g", "--group", "-C", "-D", "--chdir"]) }],
  ["nice", { valueOptions: new Set(["-n", "--adjustment"]) }],
  ["timeout", { valueOptions: new Set(["-k", "-s", "--kill-after", "--signal"]), leading: 1 }],
  ["nohup", { valueOptions: new Set() }],
  ["setsid", { valueOptions: new Set() }],
  ["time", { valueOptions: new Set() }],
  ["exec", { valueOptions: new Set(["-a"]) }],
  ["command", { valueOptions: new Set() }],
]);

const DELIMITER_LANGUAGES = new Map<string, ScriptLanguage>([
  ["py", "python"],
  ["python", "python"],
  ["sh", "bash"],
  ["bash", "bash"],
  ["shell", "bash"],
]);

const EXTENSION_LANGUAGES = new Map<string, ScriptLanguage>([
  ["py", "python"],
  ["sh", "bash"],
  ["bash", "bash"],
]);

/** Scripts embedded in a shell script, nested ones included; none when it does not parse cleanly. */
export function embeddedScripts(source: string): EmbeddedScript[] {
  const identity = Array.from({ length: source.length }, (_, i) => i);
  return scriptsIn({ text: source, starts: identity, ends: identity.map((i) => i + 1) }, 0);
}

function scriptsIn(source: MappedText, depth: number): EmbeddedScript[] {
  const script = parse(source.text);
  if (script.errors?.length) return [];
  const found: EmbeddedScript[] = [];
  forEachCommand(script, (command) => {
    for (const { language, mapped } of commandScripts(command, source.text)) {
      if (mapped.text.trim() === "") continue;
      const embedded: EmbeddedScript = {
        language,
        text: mapped.text,
        starts: mapped.starts.map((start) => source.starts[start]!),
        ends: mapped.ends.map((end) => source.ends[end - 1]!),
      };
      found.push(embedded);
      if (language === "bash" && depth + 1 < MAX_DEPTH) found.push(...scriptsIn(embedded, depth + 1));
    }
  });
  return found;
}

function hasScript(node: SyntaxNode): node is Extract<SyntaxNode, { script: ParsedScript | undefined }> {
  return "script" in node;
}

function forEachCommand(node: SyntaxNode, visit: (command: Command) => void): void {
  if (node.type === "Command") visit(node);
  // A substitution inside escaped backticks indexes its own decoded source, not ours.
  if (hasScript(node) && (!node.script || node.script.errors?.length || node.script.source !== undefined)) return;
  forEachChild(node, (child) => forEachCommand(child, visit));
}

function programName(word: Word): string {
  return word.value.slice(word.value.lastIndexOf("/") + 1);
}

function programIndex(words: readonly Word[]): number {
  let index = 0;
  while (index < words.length) {
    const wrapper = PROGRAM_WRAPPERS.get(programName(words[index]!));
    if (!wrapper) return index;
    index++;
    let optionsEnded = false;
    while (index < words.length) {
      const value = words[index]!.value;
      const isOption = !optionsEnded && value.startsWith("-");
      if (isOption && value === "--") optionsEnded = true;
      // `env -- FOO=1 cmd` still takes assignments after the options end.
      if (isOption || /^[A-Za-z_]\w*=/.test(value)) index += isOption && wrapper.valueOptions.has(value) ? 2 : 1;
      else break;
    }
    index += wrapper.leading ?? 0;
  }
  return index;
}

function commandScripts(command: Command, source: string): Array<{ language: ScriptLanguage; mapped: MappedText }> {
  if (!command.name) return [];
  const words = [command.name, ...command.args.filter((arg): arg is Word => arg.type === "Word")];
  const index = programIndex(words);
  const program = words[index];
  if (!program) return [];
  const name = programName(program);
  const interpreter = interpreterFor(name);
  const scripts: Array<{ language: ScriptLanguage; mapped: MappedText }> = [];

  let stdinLanguage: ScriptLanguage | undefined;
  if (interpreter) {
    const found = interpreterArgs(interpreter, words.slice(index + 1));
    if (found.script) {
      const mapped = wordText(found.script, source);
      if (mapped) scripts.push({ language: interpreter.language, mapped });
    } else if (!found.scriptFromFile) {
      stdinLanguage = interpreter.language;
    }
  }

  const writtenLanguage = writtenFileLanguage(name, command, words.slice(index + 1));
  for (const redirect of command.redirects) {
    const toStdin = isStdin(redirect.descriptor);
    if (redirect.type === "HereString" && toStdin && stdinLanguage && redirect.target) {
      const mapped = wordText(redirect.target, source);
      if (mapped) scripts.push({ language: stdinLanguage, mapped });
    }
    if (redirect.type === "HereDoc") {
      const language = (toStdin ? (stdinLanguage ?? writtenLanguage) : undefined) ?? delimiterLanguage(redirect);
      if (language) scripts.push({ language, mapped: hereDocText(redirect, source) });
    }
  }
  return scripts;
}

function isStdin(descriptor: HereDoc["descriptor"]): boolean {
  return descriptor === undefined || (descriptor.type === "FileDescriptor" && descriptor.value === 0);
}

/** The inline script argument, or whether the interpreter reads a script file rather than stdin. */
function interpreterArgs(interpreter: Interpreter, args: readonly Word[]): { script?: Word; scriptFromFile: boolean } {
  for (let i = 0; i < args.length; i++) {
    const flag = args[i]!.value;
    if (flag === "--" || flag === "-" || !/^[-+]/.test(flag)) {
      // The first positional argument names a script file, except `-` (stdin).
      const first = flag === "--" ? args[i + 1] : args[i];
      return { scriptFromFile: first !== undefined && first.value !== "-" };
    }
    if (interpreter.scriptFileOptions.has(flag)) return { scriptFromFile: true };
    if (interpreter.inline(flag)) {
      // Only POSIX shells skip a `--` after `-c`.
      const script = args[i + (interpreter === SHELL && args[i + 1]?.value === "--" ? 2 : 1)];
      return script ? { script, scriptFromFile: false } : { scriptFromFile: true };
    }
    if (interpreter.valueOptions.has(flag)) i++;
  }
  return { scriptFromFile: false };
}

function writtenFileLanguage(name: string, command: Command, args: readonly Word[]): ScriptLanguage | undefined {
  let path: string | undefined;
  if (name === "cat") {
    for (const redirect of command.redirects) {
      if (redirect.type === "Redirect" && [">", ">>", ">|", "&>", "&>>"].includes(redirect.operator) && (redirect.descriptor === undefined || (redirect.descriptor.type === "FileDescriptor" && redirect.descriptor.value === 1))) {
        path = redirect.target?.value;
      }
    }
  } else if (name === "tee") {
    path = args.find((arg) => !arg.value.startsWith("-"))?.value;
  }
  const ext = path?.match(/\.([A-Za-z]+)$/)?.[1]?.toLowerCase();
  return ext ? EXTENSION_LANGUAGES.get(ext) : undefined;
}

function delimiterLanguage(hereDoc: HereDoc): ScriptLanguage | undefined {
  const name = hereDoc.delimiter?.value.toLowerCase();
  return name ? DELIMITER_LANGUAGES.get(name) : undefined;
}

function hereDocText(hereDoc: HereDoc, source: string): MappedText {
  const { pos, end } = hereDoc.body;
  const starts = Array.from({ length: end - pos }, (_, i) => pos + i);
  return { text: source.slice(pos, end), starts, ends: starts.map((start) => start + 1) };
}

/** A word's shell value mapped back to its spelling; undefined when escapes do not map character for character. */
function wordText(word: Word, source: string): MappedText | undefined {
  const builder = new MappedTextBuilder();
  const parts: readonly WordPart[] = word.parts ?? [{ type: "Literal", pos: word.pos, end: word.end, text: word.text, value: word.value }];
  for (const part of parts) {
    switch (part.type) {
      case "Literal":
        if (!builder.addEscaped(part, false)) return undefined;
        break;
      case "SingleQuoted":
        builder.addRaw(part.value, part.pos + 1);
        break;
      case "DoubleQuoted":
      case "LocaleString":
        for (const child of part.parts) if (!builder.addDoubleQuotedChild(child)) return undefined;
        break;
      case "AnsiCQuoted":
        // $'…' escapes such as \x41 do not map character for character.
        if (part.value !== part.text.slice(2, -1)) return undefined;
        builder.addRaw(part.value, part.pos + 2);
        break;
      default:
        builder.addRaw(source.slice(part.pos, part.end), part.pos);
    }
  }
  return builder.build();
}

class MappedTextBuilder {
  private text = "";
  private readonly starts: number[] = [];
  private readonly ends: number[] = [];

  addRaw(text: string, pos: number) {
    for (let i = 0; i < text.length; i++) this.push(text[i]!, pos + i, 1);
  }

  addDoubleQuotedChild(child: DoubleQuotedChild): boolean {
    if (child.type === "Literal") return this.addEscaped(child, true);
    this.addRaw(child.text, child.pos);
    return true;
  }

  addEscaped(literal: { pos: number; text: string; value: string }, doubleQuoted: boolean): boolean {
    const before = this.text.length;
    const { text, pos } = literal;
    for (let i = 0; i < text.length; i++) {
      const next = text[i + 1];
      if (text[i] === "\\" && next !== undefined && (!doubleQuoted || '$`"\\\n'.includes(next))) {
        // A backslash-newline joins lines and spells nothing.
        if (next !== "\n") this.push(next, pos + i, 2);
        i++;
      } else {
        this.push(text[i]!, pos + i, 1);
      }
    }
    return this.text.slice(before) === literal.value;
  }

  build(): MappedText {
    return { text: this.text, starts: this.starts, ends: this.ends };
  }

  private push(character: string, start: number, length: number) {
    this.text += character;
    this.starts.push(start);
    this.ends.push(start + length);
  }
}

/**
 * Lay a one-line shell script out one command per line, with compound
 * commands indented. Undefined when it would change what the script says:
 * it spans lines already (its author laid it out), does not parse, or has a
 * comment (the printer drops comments).
 */
export function formatOneLiner(source: string): string | undefined {
  if (source.includes("\n")) return undefined;
  const script = parse(source);
  if (script.errors?.length || hasComment(script, source)) return undefined;
  const formatted = print(script).replace(/\n+$/, "");
  if (parse(formatted).errors?.length) return undefined;
  return formatted;
}

// Spans whose text may hold a `#` that is not a comment.
const OPAQUE = new Set(["Word", "Assignment", "HereDoc", "ArithmeticCommand", "ArithmeticFor"]);

/** Whether a `#` outside every word, heredoc and arithmetic span begins a comment. */
function hasComment(script: ParsedScript, source: string): boolean {
  const covered = new Uint8Array(source.length);
  if (script.shebang !== undefined) covered.fill(1, 0, script.shebang.length + 2);
  const walk = (node: SyntaxNode) => {
    if (OPAQUE.has(node.type)) {
      covered.fill(1, node.pos, node.end);
      return;
    }
    forEachChild(node, walk);
  };
  walk(script);
  for (let i = source.indexOf("#"); i !== -1; i = source.indexOf("#", i + 1)) if (!covered[i]) return true;
  return false;
}
