import { describe, expect, it } from "vitest";
import { commandSummary, formatBash, formatPython, parseCommand, shellJoinWords, splitShellWords } from "./command";

describe("splitShellWords", () => {
  it("losslessly round-trips POSIX single-quoted words from shell.Join", () => {
    const cases: string[][] = [
      ["cargo", "build", "--features=a,b", "it's", "", "a b"],
      ["bash", "-c", "set -x\necho 'hello world'\nexit 0"],
      ["python3", "-c", "print(\"it's\\nok\")"],
      ["echo", "'", "''", "a\\b\\c", "line1\nline2\n"],
    ];
    for (const args of cases) {
      expect(splitShellWords(shellJoinWords(args))).toEqual(args);
    }
  });

  it("handles double quotes and backslash escapes", () => {
    expect(splitShellWords(`python3 -c "print('hi')"` )).toEqual(["python3", "-c", "print('hi')"]);
    expect(splitShellWords(`echo hello\\ world`)).toEqual(["echo", "hello world"]);
  });

  it("returns null on unbalanced quotes", () => {
    expect(splitShellWords(`bash -c 'unterminated`)).toBeNull();
    expect(splitShellWords(`python3 -c "unterminated`)).toBeNull();
  });
});

describe("parseCommand", () => {
  it("unwraps a real multi-line agent bash -c command with nested single quotes", () => {
    const script = [
      "set -x",
      "export CARGO_BUILD_JOBS=4 UCD_ZIP=$PWD/target/UCD-12.1.0.zip",
      "cargo nextest --version || exit 9",
      "cargo clippy --workspace --tests -- -A clippy::all -D clippy::disallowed_methods -D warnings || exit 1",
      'madsim=(env RUSTFLAGS="--cfg madsim" CARGO_TARGET_DIR=$PWD/target/madsim)',
      '"${madsim[@]}" cargo check -p node_local -p host_fs || exit 2',
      "cargo nextest run --workspace --test-threads 4 --no-fail-fast -E 'not (package(local_cell) & test(/^sim::/))'",
      'echo "workspace exit $?"',
    ].join("\n");

    const argv = shellJoinWords(["bash", "-c", script]);
    const parsed = parseCommand(argv);
    expect(parsed.kind).toBe("script");
    if (parsed.kind !== "script") return;
    expect(parsed.language).toBe("bash");
    expect(parsed.interpreter).toBe("bash");
    expect(parsed.prefix).toEqual(["bash", "-c"]);
    expect(parsed.script).toBe(script);
    // Preserves every line and keeps `|| exit N` guards attached to their command.
    expect(parsed.formatted).toBe(script);
    expect(parsed.trailingArgs).toEqual([]);
  });

  it("unwraps shell flags (-lc, -euo pipefail -c) and wrappers (env, timeout, PATH)", () => {
    const p1 = parseCommand(shellJoinWords(["/bin/bash", "-euo", "pipefail", "-c", "echo hi", "_", "arg1"]));
    expect(p1).toMatchObject({
      kind: "script",
      language: "bash",
      interpreter: "bash",
      prefix: ["/bin/bash", "-euo", "pipefail", "-c"],
      script: "echo hi",
      trailingArgs: ["_", "arg1"],
    });

    const p2 = parseCommand(shellJoinWords(["env", "RUST_LOG=debug", "/usr/bin/sh", "-lc", "ls -la"]));
    expect(p2).toMatchObject({
      kind: "script",
      language: "bash",
      interpreter: "sh",
      prefix: ["env", "RUST_LOG=debug", "/usr/bin/sh", "-lc"],
      script: "ls -la",
    });

    const p3 = parseCommand(shellJoinWords(["timeout", "60", "zsh", "-c", "echo ok"]));
    expect(p3).toMatchObject({
      kind: "script",
      language: "bash",
      interpreter: "zsh",
      prefix: ["timeout", "60", "zsh", "-c"],
    });
  });

  it("unwraps python and python3 -c commands with flags and wrappers", () => {
    const pyScript = "import json, sys\ndata = json.load(sys.stdin)\nprint(len(data))";
    const p1 = parseCommand(shellJoinWords(["python3", "-c", pyScript, "extra"]));
    expect(p1).toMatchObject({
      kind: "script",
      language: "python",
      interpreter: "python3",
      prefix: ["python3", "-c"],
      script: pyScript,
      formatted: pyScript,
      trailingArgs: ["extra"],
    });

    const p2 = parseCommand(shellJoinWords(["python", "-u", "-B", "-c", "print(1)"]));
    expect(p2).toMatchObject({
      kind: "script",
      language: "python",
      interpreter: "python",
      prefix: ["python", "-u", "-B", "-c"],
      script: "print(1)",
    });

    const p3 = parseCommand(shellJoinWords(["/usr/bin/env", "PYTHONUNBUFFERED=1", "uv", "run", "--no-project", "python3.12", "-uc", "print(2)"]));
    expect(p3).toMatchObject({
      kind: "script",
      language: "python",
      interpreter: "python3.12",
      script: "print(2)",
    });

    // Attached `-c<script>` form
    const p4 = parseCommand(shellJoinWords(["python3", "-cprint(42)"]));
    expect(p4).toMatchObject({
      kind: "script",
      language: "python",
      interpreter: "python3",
      prefix: ["python3", "-c"],
      script: "print(42)",
    });
  });

  it("does not misclassify script files as inline scripts", () => {
    expect(parseCommand("bash scripts/run.sh").kind).toBe("command");
    expect(parseCommand("python3 scripts/madsim-seeds.py --first 1").kind).toBe("command");
    expect(parseCommand("python3 -m pytest -q").kind).toBe("command");
  });

  it("keeps short plain commands compact and wraps very long plain commands", () => {
    const short = parseCommand("cargo test -p local_cell");
    expect(short).toMatchObject({ kind: "command", compact: true, formatted: "cargo test -p local_cell" });

    const longArgv =
      "cargo clippy -p local_cell -p physmem -p cladefs -p follower -p follower_sim --features shuttle --tests -- -A clippy::all -D warnings";
    const long = parseCommand(longArgv);
    expect(long.kind).toBe("command");
    if (long.kind !== "command") return;
    expect(long.compact).toBe(false);
    expect(long.formatted).toContain(" \\\n  ");
    // Flag-value pairs stay together on the same line.
    expect(long.formatted).toContain("-p local_cell");
    expect(long.formatted).toContain("--features shuttle");
  });
});

describe("formatBash", () => {
  it("expands semicolon-separated and compound one-liners with clean indentation", () => {
    expect(formatBash("for f in *.rs; do echo \"$f\"; wc -l \"$f\"; done")).toBe(
      ['for f in *.rs; do', '  echo "$f"', '  wc -l "$f"', "done"].join("\n"),
    );

    expect(formatBash("if [ -f a ]; then echo yes; else echo no; fi")).toBe(
      ["if [ -f a ]; then", "  echo yes", "else", "  echo no", "fi"].join("\n"),
    );
  });

  it("splits long && chains cleanly without breaking quotes or subshells", () => {
    const oneLiner =
      "set -euo pipefail; cargo clippy --workspace --tests -- -D warnings && cargo nextest run --workspace --test-threads 4 && echo 'a && b; c'";
    expect(formatBash(oneLiner)).toBe(
      [
        "set -euo pipefail",
        "cargo clippy --workspace --tests -- -D warnings &&",
        "  cargo nextest run --workspace --test-threads 4 &&",
        "  echo 'a && b; c'",
      ].join("\n"),
    );
  });

  it("preserves heredocs, escaped semicolons, arrays, and case statements untouched", () => {
    const withHeredoc = [
      "cat <<'EOF' > out.txt",
      "hello && world; not split",
      "EOF",
      "find . -name '*.rs' -exec echo {} \\;",
    ].join("\n");
    expect(formatBash(withHeredoc)).toBe(withHeredoc);

    const caseLine = `case "$x" in a|b) echo 1 ;; *) echo 2 ;; esac`;
    expect(formatBash(caseLine)).toBe(caseLine);
  });

  it("dedents common leading whitespace on multi-line scripts", () => {
    expect(formatBash("\n    set -e\n    if true; then\n      echo hi\n    fi\n  ")).toBe(
      "set -e\nif true; then\n  echo hi\nfi",
    );
  });
});

describe("formatPython", () => {
  it("dedents multi-line Python without disturbing relative indentation", () => {
    const src = "\n    import sys\n    def f(x):\n        if x > 0:\n            return x * 2\n        return 0\n";
    expect(formatPython(src)).toBe(
      ["import sys", "def f(x):", "    if x > 0:", "        return x * 2", "    return 0"].join("\n"),
    );
  });

  it("expands single-line semicolon-separated Python statements", () => {
    expect(formatPython(`import json, sys; d = json.load(sys.stdin); print(json.dumps({"a": 1, "b": "x; y"}))`)).toBe(
      ["import json, sys", "d = json.load(sys.stdin)", `print(json.dumps({"a": 1, "b": "x; y"}))`].join("\n"),
    );
  });

  it("expands single-line compound Python suites while ignoring colons in dicts, slices, lambdas, and type annotations", () => {
    expect(formatPython("for i in range(3): print(i); print(i * 2)")).toBe(
      ["for i in range(3):", "    print(i)", "    print(i * 2)"].join("\n"),
    );

    expect(formatPython("def add(a: int, b: int) -> int: return a + b")).toBe(
      ["def add(a: int, b: int) -> int:", "    return a + b"].join("\n"),
    );

    expect(formatPython("if (x := len(a[1:])) > 0: print(x)")).toBe(
      ["if (x := len(a[1:])) > 0:", "    print(x)"].join("\n"),
    );

    // Non-compound statements with `:` (annotations, lambdas) stay intact.
    expect(formatPython("x: int = 1; f = lambda y: y + x; print(f(2))")).toBe(
      ["x: int = 1", "f = lambda y: y + x", "print(f(2))"].join("\n"),
    );
  });
});

describe("commandSummary", () => {
  it("produces clean single-line summaries with language badges for inline scripts", () => {
    const bash = commandSummary(shellJoinWords(["bash", "-c", "# header\nset -x\ncargo test -E 'foo'"]));
    expect(bash).toEqual({ badge: "bash", text: "set -x ; cargo test -E 'foo'" });

    const py = commandSummary(shellJoinWords(["python3", "-c", "import sys\nprint(sys.version)"]));
    expect(py).toEqual({ badge: "py", text: "import sys ; print(sys.version)" });

    const plain = commandSummary("cargo nextest run --workspace");
    expect(plain).toEqual({ badge: null, text: "cargo nextest run --workspace" });
  });
});
