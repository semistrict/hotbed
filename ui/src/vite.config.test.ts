import http from "node:http";
import type { AddressInfo } from "node:net";
import { createServer } from "vite";
import { describe, expect, it } from "vitest";
import config, { controllerProxy } from "../vite.config";

describe("vite controller proxy", () => {
  it("explicitly disables changeOrigin so the browser Host header is preserved", () => {
    const entry = config.server?.proxy?.["/hotbed.v1."];
    expect(entry).toBeTypeOf("object");
    expect(entry).toMatchObject({ changeOrigin: false });
  });

  it("forwards the incoming Host header unchanged to the controller backend", async () => {
    const seen: { host?: string; origin?: string; url?: string } = {};
    const backend = http.createServer((req, res) => {
      seen.host = req.headers.host;
      seen.origin = req.headers.origin;
      seen.url = req.url;
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
      const browserHost = `127.0.0.1:${vitePort}`;
      const browserOrigin = `http://${browserHost}`;

      const res = await fetch(`${browserOrigin}/hotbed.v1.HotbedService/Snapshot`, {
        method: "POST",
        headers: {
          "content-type": "application/json",
          origin: browserOrigin,
        },
        body: "{}",
      });
      expect(res.status).toBe(200);
      expect(seen.url).toBe("/hotbed.v1.HotbedService/Snapshot");
      expect(seen.origin).toBe(browserOrigin);
      expect(seen.host).toBe(browserHost);
      expect(seen.host).not.toBe(`127.0.0.1:${backendPort}`);
    } finally {
      await vite.close();
      await new Promise<void>((resolve) => backend.close(() => resolve()));
    }
  });
});
