/// <reference types="vitest/config" />
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// The controller serves the built UI from the hotbed binary, so dist is
// embedded by ui/embed.go. pnpm dev proxies the API to a local controller or
// dev-cockpit (127.0.0.1:7120 by default); pnpm dev:cluster defaults
// HOTBED_CONTROLLER to <controller>:7120.
export function controllerProxy(controller = process.env.HOTBED_CONTROLLER ?? "127.0.0.1:7120") {
  return {
    "/hotbed.v1.": {
      target: `http://${controller}`,
      // Preserve the browser's Host header so it matches Origin/Referer on the
      // controller's same-origin CSRF check. Vite's string target shorthand
      // sets changeOrigin: true, which rewrites Host to the backend port -> 403.
      changeOrigin: false,
    },
  };
}

export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: { outDir: "dist", emptyOutDir: true, sourcemap: false },
  server: {
    proxy: controllerProxy(),
  },
  test: { environment: "node" },
});
