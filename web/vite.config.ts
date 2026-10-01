import { defineConfig } from "vite";
import preact from "@preact/preset-vite";

// The build lands in internal/ui/dist so `go build -tags ui` can embed it.
export default defineConfig({
  plugins: [preact()],
  build: {
    outDir: "../internal/ui/dist",
    emptyOutDir: true,
    sourcemap: false,
    rollupOptions: {
      output: {
        manualChunks(id: string) {
          // The headless terminal only loads to reflow a replay drawn at
          // another size (phones, mostly); keep it out of the main xterm chunk.
          if (id.includes("@xterm/headless") || id.includes("@xterm/addon-serialize")) return "xterm-headless";
          if (id.includes("@xterm/")) return "xterm";
          return undefined;
        },
      },
    },
  },
  server: {
    port: 5173,
    proxy: {
      "/api": { target: "http://localhost:8080", ws: true, changeOrigin: false },
    },
  },
});
