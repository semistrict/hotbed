/// <reference types="vitest/config" />
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// The controller serves the built UI from the hotbed binary, so dist is
// embedded by ui/embed.go. In dev, /v1 goes to a running controller
// (HOTBED_CONTROLLER, ubu1 by default) when the UI is not using its mock.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: { outDir: "dist", emptyOutDir: true, sourcemap: false },
  server: {
    proxy: {
      "/v1": `http://${process.env.HOTBED_CONTROLLER ?? "ubu1:7120"}`,
    },
  },
  test: { environment: "node" },
});
