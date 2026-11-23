import { describe, expect, it } from "vitest";
import { formatRawWithEnv, formatShellEnv } from "./CommandBlock";

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
