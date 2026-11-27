import { afterEach, describe, expect, it, vi } from "vitest";
import { fetchJson } from "./http";

function respond(status: number, body: string) {
  const fetchMock = vi.fn(async (_path: string, _init?: RequestInit) => new Response(body, { status }));
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

afterEach(() => vi.unstubAllGlobals());

describe("fetchJson", () => {
  it("sends the session cookie and a JSON body, and returns the parsed answer", async () => {
    const fetchMock = respond(200, '{"ok":true}');
    await expect(fetchJson("/auth/org/switch", { method: "POST", body: '{"id":"o1"}' })).resolves.toEqual({ ok: true });
    const [path, init] = fetchMock.mock.calls[0]!;
    expect(path).toBe("/auth/org/switch");
    expect(init?.credentials).toBe("same-origin");
    const headers = new Headers(init?.headers);
    expect(headers.get("Accept")).toBe("application/json");
    expect(headers.get("Content-Type")).toBe("application/json");
  });

  it("leaves Content-Type off a request without a body", async () => {
    const fetchMock = respond(200, "{}");
    await fetchJson("/api/gc/status");
    expect(new Headers(fetchMock.mock.calls[0]![1]?.headers).has("Content-Type")).toBe(false);
  });

  it("throws the controller's error, else its message, else the status", async () => {
    respond(409, '{"error":"sweep running","message":"ignored"}');
    await expect(fetchJson("/api/gc/sweep")).rejects.toThrow("sweep running");
    respond(403, '{"message":"admins only"}');
    await expect(fetchJson("/admin/join-tokens")).rejects.toThrow("admins only");
    respond(502, "<html>bad gateway</html>");
    await expect(fetchJson("/auth/org/members")).rejects.toThrow("HTTP 502");
  });
});
