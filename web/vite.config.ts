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
      "/s": {
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
