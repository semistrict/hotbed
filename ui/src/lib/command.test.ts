import { describe, expect, it } from "vitest";
import { commandSummary, dedent, shellJoinWords, unwrapCommand } from "./command";

describe("shellJoinWords", () => {
  it("quotes only words with shell-special characters and escapes single quotes", () => {
    expect(shellJoinWords(["cargo", "test", "--features=a,b"])).toBe("cargo test --features=a,b");
    expect(shellJoinWords(["echo", "hello world", "it's", ""])).toBe(`echo 'hello world' 'it'\\''s' ''`);
    expect(shellJoinWords(["bash", "-c", "a\nb"])).toBe("bash -c 'a\nb'");
  });
});

describe("unwrapCommand", () => {
  it("unwraps bash/sh/zsh inline scripts from args and preserves exact verbatim script", () => {
    const script = "\n    set -euo pipefail\n    if [ -f a ]; then\n      echo 'hello && world; keep verbatim'\n    fi\n";
    const parsed = unwrapCommand(["bash", "-lc", script]);
    expect(parsed.kind).toBe("script");
    if (parsed.kind !== "script") return;
    expect(parsed.language).toBe("bash");
    expect(parsed.badge).toBe("bash");
    expect(parsed.interpreter).toBe("bash");
    expect(parsed.prefix).toEqual(["bash", "-lc"]);
    expect(parsed.script).toBe(script);
    expect(parsed.display).toBe("set -euo pipefail\nif [ -f a ]; then\n  echo 'hello && world; keep verbatim'\nfi");
    expect(parsed.trailingArgs).toEqual([]);
    expect(parsed.raw).toBe(shellJoinWords(["bash", "-lc", script]));
  });

  it("handles shell flag clusters (-euo pipefail -c) and wrappers (env, sudo, timeout)", () => {
    const p1 = unwrapCommand(["/bin/sh", "-euo", "pipefail", "-c", "echo hi; echo bye", "_", "arg1"]);
    expect(p1).toMatchObject({
      kind: "script",
      language: "bash",
      badge: "sh",
      interpreter: "sh",
      prefix: ["/bin/sh", "-euo", "pipefail", "-c"],
      script: "echo hi; echo bye",
      display: "echo hi; echo bye",
      trailingArgs: ["_", "arg1"],
    });

    const p2 = unwrapCommand(["env", "RUST_LOG=debug", "sudo", "-u", "root", "/usr/bin/bash", "-lc", "ls -la"]);
    expect(p2).toMatchObject({
      kind: "script",
      badge: "bash",
      prefix: ["env", "RUST_LOG=debug", "sudo", "-u", "root", "/usr/bin/bash", "-lc"],
      script: "ls -la",
    });

    const p3 = unwrapCommand(["timeout", "-k", "5", "60", "zsh", "-c", "echo ok"]);
    expect(p3).toMatchObject({
      kind: "script",
      badge: "zsh",
      prefix: ["timeout", "-k", "5", "60", "zsh", "-c"],
      script: "echo ok",
    });
  });

  it("unwraps python / python3 -c invocations without reformatting code", () => {
    const pyScript = "\n    import json, sys\n    for x in [1, 2]: print(x); print(x * 2)\n";
    const p1 = unwrapCommand(["python3", "-c", pyScript, "extra"]);
    expect(p1).toMatchObject({
      kind: "script",
      language: "python",
      badge: "python",
      interpreter: "python3",
      prefix: ["python3", "-c"],
      script: pyScript,
      display: "import json, sys\nfor x in [1, 2]: print(x); print(x * 2)",
      trailingArgs: ["extra"],
    });

    const p2 = unwrapCommand(["/usr/bin/env", "PYTHONUNBUFFERED=1", "python3.12", "-uc", "print(2)"]);
    expect(p2).toMatchObject({
      kind: "script",
      language: "python",
      badge: "python",
      interpreter: "python3.12",
      script: "print(2)",
    });

    const p3 = unwrapCommand(["python3", "-cprint(42)"]);
    expect(p3).toMatchObject({
      kind: "script",
      language: "python",
      badge: "python",
      prefix: ["python3", "-c"],
      script: "print(42)",
    });
  });

  it("does not misclassify script files or module invocations", () => {
    expect(unwrapCommand(["bash", "scripts/run.sh"]).kind).toBe("command");
    expect(unwrapCommand(["python3", "-m", "pytest", "-q"]).kind).toBe("command");
  });

  it("falls back to verbatim argv when args is empty or missing", () => {
    const raw = "bash -c 'for i in 1 2; do echo $i; done'";
    const parsed = unwrapCommand([], raw);
    expect(parsed).toEqual({ kind: "command", raw, compact: true });

    const longRaw = "cargo test " + "--workspace ".repeat(12);
    expect(unwrapCommand(undefined, longRaw)).toEqual({ kind: "command", raw: longRaw, compact: false });
  });

  it("prefers explicit rawArgv when provided alongside args", () => {
    const parsed = unwrapCommand(["bash", "-c", "echo hi"], "bash -c 'echo hi'");
    expect(parsed.raw).toBe("bash -c 'echo hi'");
  });

  it("does not crash on Object.prototype property names used as executables", () => {
    expect(unwrapCommand(["constructor", "--flag", "arg"])).toEqual({
      kind: "command",
      raw: "constructor --flag arg",
      compact: true,
    });
    expect(unwrapCommand(["toString", "foo"])).toEqual({
      kind: "command",
      raw: "toString foo",
      compact: true,
    });
    expect(unwrapCommand(["valueOf", "--bar"])).toEqual({
      kind: "command",
      raw: "valueOf --bar",
      compact: true,
    });
    expect(unwrapCommand(["/usr/bin/hasOwnProperty", "x"])).toEqual({
      kind: "command",
      raw: "/usr/bin/hasOwnProperty x",
      compact: true,
    });
  });
});

describe("dedent", () => {
  it("preserves relative indentation while stripping common leading whitespace", () => {
    expect(dedent("\n    def f(x):\n        return x\n")).toBe("def f(x):\n    return x");
  });
});

describe("commandSummary", () => {
  it("produces clean single-line summaries with interpreter badges when args are present", () => {
    expect(commandSummary("", ["bash", "-c", "# header\nset -x\ncargo test"])).toEqual({
      badge: "bash",
      text: "set -x ; cargo test",
    });
    expect(commandSummary("", ["python3", "-c", "import sys\nprint(sys.version)"])).toEqual({
      badge: "python",
      text: "import sys ; print(sys.version)",
    });
    expect(commandSummary("cargo nextest run --workspace")).toEqual({
      badge: null,
      text: "cargo nextest run --workspace",
    });
  });
});
