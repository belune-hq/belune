import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { tanstackRouter } from "@tanstack/router-plugin/vite";
import path from "path";

export default defineConfig({
  plugins: [
    tanstackRouter({
      routesDirectory: "./src/routes",
      generatedRouteTree: "./src/routeTree.gen.ts",
      autoCodeSplitting: true,
    }),
    react(),
    tailwindcss(),
  ],
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
  },
  server: {
    host: true,
    proxy: {
      "/api": {
        target: "http://localhost:8080",
        changeOrigin: true,
        ws: true,
        configure: (proxy) => {
          proxy.on("error", (err) => {
            if ((err as NodeJS.ErrnoException).code !== "EPIPE") {
              console.error("[proxy error]", err);
            }
          });
        },
      },
      // The production binary serves /mcp from the same origin as the SPA,
      // which is what "Connect an AI Assistant" assumes when it builds its
      // command from window.location.origin. Without this, that command
      // points at the Vite dev server (5173) instead of the API (8080) and
      // 404s the moment someone actually runs it in dev.
      "/mcp": {
        target: "http://localhost:8080",
        changeOrigin: true,
      },
    },
  },
  test: {
    // Two projects rather than one jsdom environment for everything, so the
    // pure-logic tests — which are the majority and the fastest — do not pay
    // jsdom's startup per file, and so DOM globals cannot quietly become
    // available to a test that was meant to prove something about a pure
    // function.
    //
    // The split is by EXTENSION, not by an exclude pattern: overriding
    // `exclude` would also discard vitest's own defaults, node_modules among
    // them. A test that renders anything needs JSX and is therefore .tsx
    // already, which makes the two include globs naturally disjoint.
    //
    // Consequence worth knowing: a DOM test written as plain .ts lands in the
    // `unit` project and fails for want of a `document`. Name it .tsx.
    projects: [
      {
        extends: true,
        test: {
          name: "unit",
          environment: "node",
          include: ["src/**/*.test.ts"],
        },
      },
      {
        extends: true,
        test: {
          name: "dom",
          environment: "jsdom",
          include: ["src/**/*.test.tsx"],
          setupFiles: ["./src/test/setup-dom.ts"],
        },
      },
    ],
  },
  build: {
    outDir: "build",
    rollupOptions: {
      output: {
        manualChunks(id) {
          if (!id.includes("/node_modules/")) return;
          if (
            id.includes("/node_modules/react/") ||
            id.includes("/node_modules/react-dom/") ||
            id.includes("/node_modules/scheduler/")
          ) {
            return "react-vendor";
          }
          if (id.includes("/node_modules/@tanstack/")) {
            return "tanstack";
          }
          // The editor is only ever imported lazily (see code-editor.tsx), so
          // naming the chunk does not bring it into the first load — the entry
          // does not reference it. What it buys is cache stability: without
          // this the editor's chunk imports the entry chunk, so its hash
          // changes with every release and a ~300 kB download repeats after
          // every update.sh. As a vendor chunk with no imports of ours, it
          // changes only when CodeMirror does, or when the app first uses
          // another of its exports (a one-time change of a few bytes).
          //
          // The embedded binary is the same size either way, since
          // `//go:embed all:dist` takes every chunk: this is about first paint
          // and repeat downloads, not image size.
          if (
            id.includes("/node_modules/@codemirror/") ||
            id.includes("/node_modules/@lezer/") ||
            id.includes("/node_modules/@marijn/") ||
            id.includes("/node_modules/style-mod/") ||
            id.includes("/node_modules/w3c-keyname/") ||
            id.includes("/node_modules/crelt/")
          ) {
            return "codemirror";
          }
        },
      },
    },
  },
});
