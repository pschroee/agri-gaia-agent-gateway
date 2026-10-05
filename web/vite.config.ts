import path from "node:path"
import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"

export default defineConfig({
  // Relative Adressen: Die UI läuft unter / (lokal) und hinter einem Proxy unter einem Pfadpräfix
  // (https://app.<basis>/agent/). Dank Hash-Routing ist der Dokumentpfad immer der Einstiegspunkt.
  base: "./",
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": path.resolve(import.meta.dirname, "src"),
    },
  },
  // ./dev.sh start: Vite liefert die UI mit Hot Reload und reicht API und Anmeldung an den
  // Orchestrator weiter. changeOrigin, weil der Orchestrator nur bekannte Host-Kopfzeilen annimmt.
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
