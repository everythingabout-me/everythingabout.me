import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { RouterProvider } from "react-router/dom";
import { htmlLang, t } from "./i18n";
import { router } from "./router";
import "./styles/globals.css";

// Language follows the system preference only, with no switcher; sync the document language
// and description before first paint.
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
