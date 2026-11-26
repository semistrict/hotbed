// Syntax highlighting for shell and Python scripts. Loaded lazily (see
// useHighlighter) so the grammars stay out of the main bundle.
//
// Shiki's JavaScript regex engine, not its Oniguruma WASM one: the cockpit's
// CSP has no 'wasm-unsafe-eval'. The CSS-variables theme colors tokens with
// --shiki-* variables that styles.css maps onto the cockpit's own palette, so
// light and dark mode need no second pass.

import { createCssVariablesTheme, createHighlighterCoreSync } from "shiki/core";
import { createJavaScriptRegexEngine } from "shiki/engine/javascript";
import python from "shiki/langs/python.mjs";
import shellscript from "shiki/langs/shellscript.mjs";
import type { ScriptLanguage } from "./command";
import { embeddedScripts, type EmbeddedScript } from "./shell";

export { formatOneLiner } from "./shell";

export interface Token {
  readonly content: string;
  readonly color?: string;
  /** Shiki's FontStyle bit set: 1 italic, 2 bold, 4 underline. */
  readonly fontStyle?: number;
}

const theme = createCssVariablesTheme({ name: "cockpit", variablePrefix: "--shiki-", fontStyle: true });

const highlighter = createHighlighterCoreSync({
  themes: [theme],
  langs: [shellscript, python],
  engine: createJavaScriptRegexEngine(),
});

const GRAMMAR: Record<ScriptLanguage, string> = { bash: "shellscript", python: "python" };

interface Style {
  readonly color?: string;
  readonly fontStyle?: number;
}

/** Tokens per line; a shell script's embedded scripts are highlighted in their own languages. */
export function highlight(code: string, language: ScriptLanguage): Token[][] {
  // One style per character, so an embedded script can repaint its span.
  const styles = Array.from<Style | undefined>({ length: code.length });
  for (const t of tokenize(code, language)) styles.fill(t, t.offset, t.offset + t.content.length);
  // embeddedScripts lists a nested script after the one holding it, so inner ones paint last.
  if (language === "bash") for (const script of embeddedScripts(code)) paint(styles, script);
  return splitLines(code, styles);
}

function tokenize(code: string, language: ScriptLanguage): Array<{ offset: number; content: string } & Style> {
  return highlighter.codeToTokens(code, { lang: GRAMMAR[language], theme: "cockpit" }).tokens.flat();
}

function paint(styles: Array<Style | undefined>, script: EmbeddedScript) {
  for (const t of tokenize(script.text, script.language)) {
    if (t.content === "") continue;
    styles.fill(t, script.starts[t.offset], script.ends[t.offset + t.content.length - 1]);
  }
}

function splitLines(code: string, styles: ReadonlyArray<Style | undefined>): Token[][] {
  const lines: Token[][] = [];
  let start = 0;
  for (const lineBreak of [...code.matchAll(/\r?\n/g), undefined]) {
    const end = lineBreak?.index ?? code.length;
    const tokens: Token[] = [];
    for (let i = start; i < end; ) {
      const style = styles[i];
      let next = i + 1;
      while (next < end && styles[next]?.color === style?.color && styles[next]?.fontStyle === style?.fontStyle) next++;
      tokens.push({ content: code.slice(i, next), ...(style?.color ? { color: style.color } : {}), ...(style?.fontStyle ? { fontStyle: style.fontStyle } : {}) });
      i = next;
    }
    lines.push(tokens);
    if (lineBreak) start = lineBreak.index + lineBreak[0].length;
  }
  return lines;
}
