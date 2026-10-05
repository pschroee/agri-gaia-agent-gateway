import path from "node:path"
import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"

export default defineConfig({
  // Relative addresses: the UI runs under / (locally) and behind a proxy under a path prefix
  // (https://app.<base>/agent/). Thanks to hash routing the document path is always the entry point.
  base: "./",
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": path.resolve(import.meta.dirname, "src"),
    },
  },
  // ./dev.sh start: Vite serves the UI with hot reload and forwards the API and login to the
  // orchestrator. changeOrigin because the orchestrator only accepts known Host headers.
  server: {
    proxy: {
      "/api": { target: "http://127.0.0.1:18480", changeOrigin: true },
      "/login": { target: "http://127.0.0.1:18480", changeOrigin: true },
      "/oidc": { target: "http://127.0.0.1:18480", changeOrigin: true },
    },
  },
  build: {
    outDir: "dist",
  },
})
