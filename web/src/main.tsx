import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { RouterProvider } from "react-router/dom";
import { htmlLang, t } from "./i18n";
import { router } from "./router";
import "./styles/globals.css";

// 语言只跟随系统偏好，没有切换入口；在首屏渲染前同步好文档语言和描述。
document.documentElement.lang = htmlLang;
const description = document.querySelector('meta[name="description"]');
if (description) description.setAttribute("content", t("meta.description"));

const container = document.getElementById("root");
if (!container) throw new Error("could not find the #root mount point");

createRoot(container).render(
  <StrictMode>
    <RouterProvider router={router} />
  </StrictMode>,
);
