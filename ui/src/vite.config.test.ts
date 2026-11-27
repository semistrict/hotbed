import http from "node:http";
import type { AddressInfo } from "node:net";
import { createServer } from "vite";
import { afterEach, describe, expect, it, vi } from "vitest";
import config, { controllerProxy } from "../vite.config";

describe("vite controller proxy", () => {
  afterEach(() => {
    vi.unstubAllEnvs();
  });

  it("explicitly disables changeOrigin so the browser Host header is preserved", () => {
    vi.stubEnv("HOTBED_CONTROLLER", "127.0.0.1:7120");
    const entry = config({ command: "serve", mode: "development" }).server?.proxy?.["/hotbed.v1."];
    expect(entry).toMatchObject({ target: "http://127.0.0.1:7120", changeOrigin: false });
  });

  it("refuses the CLI's HTTPS controller URL with what to run instead, and only when serving", () => {
    vi.stubEnv("HOTBED_CONTROLLER", "https://hotbed.example.com");
    expect(() => config({ command: "serve", mode: "development" })).toThrow(
      "HOTBED_CONTROLLER=https://hotbed.example.com: the UI dev proxy needs a controller over plain HTTP, as host:port. Run `make dev-cockpit`, then `HOTBED_CONTROLLER=127.0.0.1:7120 pnpm -C ui dev`.",
    );
    expect(config({ command: "build", mode: "production" }).server).toEqual({});
    expect(controllerProxy("http://127.0.0.1:7120")["/auth/"].target).toBe("http://127.0.0.1:7120");
  });

  it("forwards the incoming Host header unchanged to the controller backend", async () => {
    await throughProxy(async (browserOrigin, seen, backendPort) => {
      const browserHost = new URL(browserOrigin).host;
      const res = await fetch(`${browserOrigin}/hotbed.v1.HotbedService/Snapshot`, {
        method: "POST",
        headers: {
          "content-type": "application/json",
          origin: browserOrigin,
        },
        body: "{}",
      });
      expect(res.status).toBe(200);
      expect(seen[0]?.url).toBe("/hotbed.v1.HotbedService/Snapshot");
      expect(seen[0]?.origin).toBe(browserOrigin);
      expect(seen[0]?.host).toBe(browserHost);
      expect(seen[0]?.host).not.toBe(`127.0.0.1:${backendPort}`);
    });
  });

  it("forwards each of the controller's routes: Connect RPCs, /auth/, /admin/ and /api/", async () => {
    expect(Object.keys(controllerProxy("127.0.0.1:7120"))).toEqual(["/hotbed.v1.", "/auth/", "/admin/", "/api/"]);
    await throughProxy(async (browserOrigin, seen) => {
      const paths = ["/hotbed.v1.ClusterService/GetSnapshot", "/auth/me", "/admin/join-tokens", "/api/gc/status"];
      for (const path of paths) expect((await fetch(`${browserOrigin}${path}`)).status).toBe(200);
      expect(seen.map((r) => r.url)).toEqual(paths);
    });
  });
});

interface Seen {
  host?: string;
  origin?: string;
  url?: string;
}

/** Runs fn against a Vite dev server whose controller proxy points at a backend that records each request it gets. */
async function throughProxy(fn: (browserOrigin: string, seen: Seen[], backendPort: number) => Promise<void>) {
  const seen: Seen[] = [];
  const backend = http.createServer((req, res) => {
    seen.push({ host: req.headers.host, origin: req.headers.origin, url: req.url });
    res.writeHead(200, { "content-type": "application/json" });
    res.end("{}");
  });
  await new Promise<void>((resolve) => backend.listen(0, "127.0.0.1", resolve));
  const backendPort = (backend.address() as AddressInfo).port;

  const vite = await createServer({
    configFile: false,
    logLevel: "silent",
    server: {
      host: "127.0.0.1",
      port: 0,
      proxy: controllerProxy(`127.0.0.1:${backendPort}`),
    },
  });
  try {
    await vite.listen();
    const vitePort = (vite.httpServer!.address() as AddressInfo).port;
    await fn(`http://127.0.0.1:${vitePort}`, seen, backendPort);
  } finally {
    await vite.close();
    await new Promise<void>((resolve) => backend.close(() => resolve()));
  }
}
