import { useEffect, useMemo, useState, type CSSProperties } from "react";
import type { ScriptLanguage } from "../lib/command";
import type { Token } from "../lib/highlight";

type HighlightModule = typeof import("../lib/highlight");

let loaded: HighlightModule | undefined;
let loading: Promise<HighlightModule> | undefined;

/** The highlighting module once its lazy chunk has loaded; undefined until then. */
export function useHighlighter(): HighlightModule | undefined {
  const [module, setModule] = useState(loaded);
  useEffect(() => {
    if (module) return;
    let live = true;
    loading ??= import("../lib/highlight").then((m) => (loaded = m));
    loading.then(
      (m) => live && setModule(m),
      (err: unknown) => console.error("loading the syntax highlighter failed", err),
    );
    return () => {
      live = false;
    };
  }, [module]);
  return module;
}

function tokenStyle(color?: string, fontStyle = 0): CSSProperties {
  return {
    ...(color ? { color } : {}),
    ...(fontStyle & 1 ? { fontStyle: "italic" } : {}),
    ...(fontStyle & 2 ? { fontWeight: 600 } : {}),
    ...(fontStyle & 4 ? { textDecoration: "underline" } : {}),
  };
}

/**
 * Code in a <pre>, one row per line with an optional line-number gutter,
 * syntax-highlighted once the highlighter loads (plain until then, in the
 * same layout). Shows the first `limit` lines when given.
 */
export function CodeLines({
  code,
  language,
  gutter,
  limit,
  className,
  lineClassName,
}: {
  code: string;
  language: ScriptLanguage;
  gutter: boolean;
  limit?: number;
  className: string;
  lineClassName: string;
}) {
  const highlighter = useHighlighter();
  const lines = useMemo<readonly (readonly Token[])[]>(
    () => (highlighter ? highlighter.highlight(code, language) : code.split("\n").map((content) => [{ content }])),
    [highlighter, code, language],
  );
  const shown = limit === undefined ? lines : lines.slice(0, limit);
  return (
    <pre className={`m-0 overflow-x-auto font-mono leading-[1.65] text-foreground ${className}`}>
      <code>
        {shown.map((tokens, idx) => (
          <span key={idx} className="flex hover:bg-muted/35">
            {gutter && <span className="w-10 shrink-0 pr-3 text-right text-[10.5px] leading-[1.9] select-none text-faint">{idx + 1}</span>}
            <span className={`min-w-0 flex-1 whitespace-pre-wrap break-words ${gutter ? "" : "pl-3"} ${lineClassName}`}>
              {tokens.length === 0 || (tokens.length === 1 && tokens[0]!.content === "")
                ? " "
                : tokens.map((t, i) => (
                    <span key={i} style={tokenStyle(t.color, t.fontStyle)}>
                      {t.content}
                    </span>
                  ))}
            </span>
          </span>
        ))}
      </code>
    </pre>
  );
}
