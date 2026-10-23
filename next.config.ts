import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  // 用 127.0.0.1 或局域网 IP 打开 dev server 时，Next 16 默认会拦掉 /_next/hmr，
  // 表现是页面一直停在骨架屏（hydration 不执行）。
  allowedDevOrigins: ["127.0.0.1", "localhost"],
};

export default nextConfig;
