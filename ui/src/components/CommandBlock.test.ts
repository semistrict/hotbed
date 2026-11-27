import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { afterEach, describe, expect, it, vi } from "vitest";
import { CommandBlock, formatRawWithEnv, formatShellEnv } from "./CommandBlock";
import { copyToClipboard } from "./CopyButton";

describe("formatShellEnv", () => {
  it("keeps simple KEY=VAL assignments unquoted and quotes values with shell-special characters", () => {
    expect(formatShellEnv("FOO=bar")).toBe("FOO=bar");
    expect(formatShellEnv("MESSAGE=hello world")).toBe("MESSAGE='hello world'");
    expect(formatShellEnv("QUOTE=it's fine")).toBe("QUOTE='it'\\''s fine'");
    expect(formatShellEnv("EMPTY=")).toBe("EMPTY=''");
    expect(formatShellEnv("EXPR=a=b c")).toBe("EXPR='a=b c'");
    expect(formatShellEnv("EXPR=a=b")).toBe("EXPR=a=b");
  });

  it("shell-quotes entries without an equals sign or with non-identifier keys", () => {
    expect(formatShellEnv("BARE")).toBe("BARE");
    expect(formatShellEnv("has space")).toBe("'has space'");
    expect(formatShellEnv("bad key=val")).toBe("'bad key=val'");
    expect(formatShellEnv("=val")).toBe("=val");
  });
});

describe("formatRawWithEnv", () => {
  it("prepends shell-quoted env assignments to the raw command", () => {
    expect(formatRawWithEnv("echo $MESSAGE", ["MESSAGE=hello world"])).toBe("MESSAGE='hello world' echo $MESSAGE");
    expect(formatRawWithEnv("cargo test", ["FOO=bar", "RUST_LOG=info,hotbed=debug"])).toBe("FOO=bar RUST_LOG=info,hotbed=debug cargo test");
    expect(formatRawWithEnv("echo hi", ["FOO=bar", "MSG=a b", "Q=x'y"])).toBe("FOO=bar MSG='a b' Q='x'\\''y' echo hi");
  });

  it("returns raw unchanged when env is empty or omitted", () => {
    expect(formatRawWithEnv("go test ./...")).toBe("go test ./...");
    expect(formatRawWithEnv("go test ./...", [])).toBe("go test ./...");
  });
});

describe("CommandBlock layout and semantics", () => {
  it("always renders an <h1> in hero variant (plain, inline script, multi-line, and env vars)", () => {
    const plain = renderToStaticMarkup(createElement(CommandBlock, { argv: "go test ./...", variant: "hero" }));
    const script = renderToStaticMarkup(
      createElement(CommandBlock, { args: ["bash", "-c", "echo hi\necho bye"], variant: "hero" }),
    );
    const multiline = renderToStaticMarkup(
      createElement(CommandBlock, { argv: "echo one\necho two", variant: "hero" }),
    );
    const withEnv = renderToStaticMarkup(
      createElement(CommandBlock, { argv: "go test ./...", env: ["CGO_ENABLED=0"], variant: "hero" }),
    );

    expect(plain).toContain("<h1");
    expect(script).toContain("<h1");
    expect(multiline).toContain("<h1");
    expect(withEnv).toContain("<h1");
  });

  it("keeps overflow-x-auto on <code> and uses muted fade gradient in compact variant", () => {
    const shortCompact = renderToStaticMarkup(
      createElement(CommandBlock, { argv: "go test ./...", variant: "compact" }),
    );
    expect(shortCompact).not.toContain("<h1");
    expect(shortCompact).toMatch(/<code class="[^"]*min-w-0 flex-1 overflow-x-auto/);
    expect(shortCompact).not.toMatch(/<div class="[^"]*overflow-x-auto[^"]*"><code/);

    const tallCompact = renderToStaticMarkup(
      createElement(CommandBlock, {
        argv: Array.from({ length: 12 }, (_, i) => `echo line ${i}`).join("\n"),
        variant: "compact",
        maxLines: 6,
      }),
    );
    expect(tallCompact).toContain("from-muted/95");
    expect(tallCompact).not.toContain("from-card/95");
  });
});

describe("copyToClipboard", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("uses navigator.clipboard.writeText when available", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal("navigator", { clipboard: { writeText } });
    expect(await copyToClipboard("hello")).toBe(true);
    expect(writeText).toHaveBeenCalledWith("hello");
  });

  it("falls back to document.execCommand('copy') when clipboard API is absent or rejects", async () => {
    const writeText = vi.fn().mockRejectedValue(new Error("insecure context"));
    vi.stubGlobal("navigator", { clipboard: { writeText } });

    const appended: unknown[] = [];
    const removed: unknown[] = [];
    const execCommand = vi.fn().mockReturnValue(true);
    const fakeTextarea = {
      value: "",
      style: {} as Record<string, string>,
      setAttribute: vi.fn(),
      select: vi.fn(),
      setSelectionRange: vi.fn(),
    };
    vi.stubGlobal("document", {
      execCommand,
      createElement: () => fakeTextarea,
      body: {
        appendChild: (el: unknown) => appended.push(el),
        removeChild: (el: unknown) => removed.push(el),
      },
    });

    expect(await copyToClipboard("cmd --flag")).toBe(true);
    expect(execCommand).toHaveBeenCalledWith("copy");
    expect(fakeTextarea.value).toBe("cmd --flag");
    expect(appended).toHaveLength(1);
    expect(removed).toHaveLength(1);
  });

  it("returns false cleanly when neither clipboard API nor execCommand is available", async () => {
    vi.stubGlobal("navigator", {});
    vi.stubGlobal("document", undefined);
    expect(await copyToClipboard("nope")).toBe(false);
  });
});
