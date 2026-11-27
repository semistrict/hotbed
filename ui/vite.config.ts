/// <reference types="vitest/config" />
import { fileURLToPath, URL } from "node:url";
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// The cf-controller Worker serves the built UI (ui/dist) as static assets.
// pnpm dev proxies the API to a local controller or dev-cockpit
// (127.0.0.1:7120 by default); pnpm dev:cluster defaults HOTBED_CONTROLLER to
// <controller>:7120.
export function controllerProxy(controller = process.env.HOTBED_CONTROLLER ?? "127.0.0.1:7120") {
  // HOTBED_CONTROLLER is the CLI's too, where it is usually the cluster's
  // HTTPS URL. Keeping the browser's Host header (below) only passes the
  // same-origin check of a controller reached over plain HTTP.
  const target = controller.includes("://") ? controller : `http://${controller}`;
  if (!target.startsWith("http://")) {
    throw new Error(
      `HOTBED_CONTROLLER=${controller}: the UI dev proxy needs a controller over plain HTTP, as host:port. Run \`make dev-cockpit\`, then \`HOTBED_CONTROLLER=127.0.0.1:7120 pnpm -C ui dev\`.`,
    );
  }
  const entry = {
    target,
    // Preserve the browser's Host header so it matches Origin/Referer on the
    // controller's same-origin CSRF check. Vite's string target shorthand
    // sets changeOrigin: true, which rewrites Host to the backend port -> 403.
    changeOrigin: false,
  };
  return {
    "/hotbed.v1.": entry,
    "/auth/": entry,
    "/admin/": entry,
    "/api/": entry,
  };
}

export default defineConfig(({ command, mode }) => ({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": fileURLToPath(new URL("./src", import.meta.url)),
    },
  },
  build: { outDir: "dist", emptyOutDir: true, sourcemap: false },
  // Only the dev server proxies: a build or a test run never needs a controller.
  server: command === "serve" && mode !== "test" ? { proxy: controllerProxy() } : {},
  test: { environment: "node" },
}));
