import { describe, expect, it } from "vitest";
import { highlight } from "./highlight";

const render = (lines: ReturnType<typeof highlight>) => lines.map((l) => l.map((t) => (t.color ? `[${t.content}|${t.color.replace(/^var\(--shiki-(token-)?|\)$/g, "")}]` : t.content)).join(""));

describe("highlight", () => {
  it("highlights python nested in bash -c nested in a shell command line", () => {
    expect(render(highlight(`bash -c "cd /x && python3 -c 'import sys; print(sys.argv)'"`, "bash"))).toEqual([
      `[bash|function][ |foreground][-c|string][ |foreground]["|string-expression][cd|function][ |foreground][/x|string][ |foreground][&&|punctuation][ |foreground][python3|function][ |foreground][-c|string][ |foreground]['|string-expression][import|keyword][ sys; |foreground][print|function][(sys.argv)|punctuation]['"|string-expression]`,
    ]);
  });

  it("highlights a python heredoc in a script", () => {
    expect(render(highlight("set -e\npython3 - <<'PY'\nimport os\nprint(os.getcwd())\nPY", "bash"))).toEqual([
      "[set|function][ |foreground][-e|string]",
      "[python3|function][ |foreground][-|string][ |foreground][<<|keyword]['PY'|foreground]",
      "[import|keyword][ os|foreground]",
      "[print|function][(os.|punctuation][getcwd|function][())|punctuation]",
      "[PY|foreground]",
    ]);
  });
});
