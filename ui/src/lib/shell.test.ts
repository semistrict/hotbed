import { describe, expect, it } from "vitest";
import { embeddedScripts, formatOneLiner } from "./shell";

/** The source spelling each embedded script spans, start to end. */
function spans(source: string) {
  return embeddedScripts(source).map((s) => ({ language: s.language, text: s.text, spelled: source.slice(s.starts[0], s.ends.at(-1)) }));
}

describe("embeddedScripts", () => {
  it("finds python -c inside a shell script, through wrappers", () => {
    expect(spans(`cd /src && timeout -k 5 60 env FOO=1 python3 -u -c 'import sys; print(sys.argv)' a`)).toEqual([
      { language: "python", text: "import sys; print(sys.argv)", spelled: "import sys; print(sys.argv)" },
    ]);
  });

  it("finds python nested in bash -c nested in the script, mapping escapes back to their spelling", () => {
    const source = `bash -c "python3 -c \\"print('hi')\\""`;
    expect(spans(source)).toEqual([
      { language: "bash", text: `python3 -c "print('hi')"`, spelled: `python3 -c \\"print('hi')\\"` },
      { language: "python", text: "print('hi')", spelled: "print('hi')" },
    ]);
  });

  it("finds python in a heredoc fed to the interpreter, written to a .py file, or named by its delimiter", () => {
    expect(spans("python3 - <<'EOF'\nprint(1)\nEOF\ncat > x.py <<EOF\nprint(2)\nEOF\nsomething <<PY\nprint(3)\nPY\n")).toEqual([
      { language: "python", text: "print(1)\n", spelled: "print(1)\n" },
      { language: "python", text: "print(2)\n", spelled: "print(2)\n" },
      { language: "python", text: "print(3)\n", spelled: "print(3)\n" },
    ]);
  });

  it("finds python in a command substitution", () => {
    expect(spans(`v=$(python3 -c 'print(1)')`)).toEqual([{ language: "python", text: "print(1)", spelled: "print(1)" }]);
  });

  it("ignores interpreters running a script file or module", () => {
    expect(spans("python3 -m pytest -c conf.ini <<EOF\nx\nEOF\nbash run.sh -c x")).toEqual([]);
  });

  it("finds nothing in a script that does not parse", () => {
    expect(spans(`python3 -c 'print(1)`)).toEqual([]);
  });
});

describe("formatOneLiner", () => {
  it("puts each command on its own line and indents compound commands", () => {
    expect(formatOneLiner("set -e; cd /x && make -j4 || { echo fail; exit 1; }; for f in a b; do echo $f; done")).toBe(
      ["set -e", "cd /x && make -j4 || {", "  echo fail", "  exit 1", "}", "for f in a b; do", "  echo $f", "done"].join("\n"),
    );
  });

  it("keeps quoting, expansions and nested scripts as written", () => {
    expect(formatOneLiner(`x=$(echo "a#b" \${#y} $#); python3 -c 'import sys; print(1)' "$x"`)).toBe(
      [`x=$(echo "a#b" \${#y} $#)`, `python3 -c 'import sys; print(1)' "$x"`].join("\n"),
    );
  });

  it("leaves a script with a comment, a multi-line script, or one that does not parse alone", () => {
    expect(formatOneLiner("make; make test # slow")).toBeUndefined();
    expect(formatOneLiner("make\nmake test")).toBeUndefined();
    expect(formatOneLiner("if true; then echo")).toBeUndefined();
  });
});
