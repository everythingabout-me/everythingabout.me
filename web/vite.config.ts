import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  server: {
    host: "127.0.0.1",
    port: 5173,
    // 端口固定：COS 的 CORS 允许来源是写死的，静默换端口会让直传被拦
    strictPort: true,
    // 开发时前端跑在 Vite，接口代理给 Go 后端；生产由 Go 直接托管 dist。
    proxy: {
      "/api": {
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
