import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  server: {
    host: "127.0.0.1",
    port: 5173,
    // Port is fixed: the COS CORS allowlist is hard-coded, so a silently different port
    // would block direct uploads.
    strictPort: true,
    // In dev the frontend runs on Vite and proxies the API to Go; in production Go serves dist.
    proxy: {
      "/api": {
        target: "http://127.0.0.1:8080",
        changeOrigin: false,
      },
      // Save links are server-rendered, so in dev they must reach Go too, not Vite's SPA fallback.
      // A key starting with "^" is matched as a RegExp. The bare prefix "/s" would also match
      // Vite's own "/src/**" module URLs (the matcher is a plain startsWith) and proxy the whole
      // app's source to Go, which answers every module request with 404.
      "^/s/": {
        target: "http://127.0.0.1:8080",
        changeOrigin: false,
      },
      // The public API also has resources at the origin root. The generated /s/ instructions
      // call /me and /events, so without these entries those requests would hit Vite's SPA
      // fallback in development while working on the single-origin production deployment.
      "^/me$": {
        target: "http://127.0.0.1:8080",
        changeOrigin: false,
      },
      "^/events": {
        target: "http://127.0.0.1:8080",
        changeOrigin: false,
      },
    },
  },
  build: {
    outDir: "dist",
    sourcemap: false,
  },
});
