import { describe, expect, it } from "vitest";
import { tokenize, tokenizeBash, tokenizePython, type Token } from "./highlight";

function reconstruct(lines: Token[][]): string {
  return lines.map((line) => line.map((t) => t.text).join("")).join("\n");
}

describe("tokenizeBash", () => {
  it("preserves every character losslessly across complex bash constructs", () => {
    const samples = [
      "",
      "cargo test --workspace --test-threads=4",
      'export CARGO_BUILD_JOBS=4 UCD_ZIP=$PWD/target/UCD-12.1.0.zip\ncargo clippy --tests -- -D warnings || exit 1\necho "workspace exit $?"',
      "cat <<'EOF' 2>&1\nhello $WORLD\nEOF\n# trailing comment\nfor x in a b; do\n  echo $'line1\\nline2'\ndone",
      "madsim=(env RUSTFLAGS=\"--cfg madsim\" CARGO_TARGET_DIR=$PWD/target/madsim)\n\"${madsim[@]}\" scripts/madsim-seeds.py --first 1 --count 5",
    ];
    for (const sample of samples) {
      expect(reconstruct(tokenizeBash(sample))).toBe(sample);
    }
  });

  it("classifies keywords, builtins, flags, variables, strings, comments, and operators", () => {
    const lines = tokenizeBash('export FOO=42\ncargo clippy --tests -- -D warnings || exit 1 # check\necho "hi ${USER}"');
    expect(lines).toHaveLength(3);

    expect(lines[0]).toEqual([
      { kind: "builtin", text: "export" },
      { kind: "plain", text: " " },
      { kind: "variable", text: "FOO" },
      { kind: "operator", text: "=" },
      { kind: "number", text: "42" },
    ]);

    expect(lines[1]?.some((t) => t.kind === "command" && t.text === "cargo")).toBe(true);
    expect(lines[1]?.some((t) => t.kind === "flag" && t.text === "--tests")).toBe(true);
    expect(lines[1]?.some((t) => t.kind === "operator" && t.text === "||")).toBe(true);
    expect(lines[1]?.some((t) => t.kind === "builtin" && t.text === "exit")).toBe(true);
    expect(lines[1]?.some((t) => t.kind === "comment" && t.text === "# check")).toBe(true);

    // Variable inside double-quoted string is highlighted as variable.
    expect(lines[2]?.some((t) => t.kind === "variable" && t.text === "${USER}")).toBe(true);
    expect(lines[2]?.some((t) => t.kind === "string" && t.text.includes("hi "))).toBe(true);
  });
});

describe("tokenizePython", () => {
  it("preserves every character losslessly across complex Python constructs", () => {
    const samples = [
      "",
      'import json, sys\n@dataclass\nclass Item:\n    name: str\n\ndef run(xs: list[int]) -> int:\n    """multi\n    line\n    doc"""\n    total = 0x10 + 3.5\n    for x in xs:\n        if x is not None:\n            print(f"val={x}") # inline\n    return total',
      "r'raw\\string' + b\"bytes\" + '''triple\\nquote'''",
    ];
    for (const sample of samples) {
      expect(reconstruct(tokenizePython(sample))).toBe(sample);
    }
  });

  it("classifies keywords, decorators, functions, strings, builtins, constants, numbers, and comments", () => {
    const code = [
      "import sys",
      "@staticmethod",
      "def greet(name: str) -> None:",
      '    print(f"hello {name}", 42, True) # done',
    ].join("\n");
    const lines = tokenize(code, "python");
    expect(lines).toHaveLength(4);
    expect(reconstruct(lines)).toBe(code);

    expect(lines[0]).toEqual([
      { kind: "keyword", text: "import" },
      { kind: "plain", text: " sys" },
    ]);
    expect(lines[1]).toEqual([{ kind: "decorator", text: "@staticmethod" }]);
    expect(lines[2]?.some((t) => t.kind === "keyword" && t.text === "def")).toBe(true);
    expect(lines[2]?.some((t) => t.kind === "function" && t.text === "greet")).toBe(true);
    expect(lines[2]?.some((t) => t.kind === "variable" && t.text === "None")).toBe(true);
    expect(lines[3]?.some((t) => t.kind === "builtin" && t.text === "print")).toBe(true);
    expect(lines[3]?.some((t) => t.kind === "string" && t.text === 'f"hello {name}"')).toBe(true);
    expect(lines[3]?.some((t) => t.kind === "number" && t.text === "42")).toBe(true);
    expect(lines[3]?.some((t) => t.kind === "variable" && t.text === "True")).toBe(true);
    expect(lines[3]?.some((t) => t.kind === "comment" && t.text === "# done")).toBe(true);
  });
});
