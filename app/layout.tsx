import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
  title: "Everything About Me",
  description: "个人数据基座",
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="zh-CN">
      <body>{children}</body>
    </html>
  );
}
