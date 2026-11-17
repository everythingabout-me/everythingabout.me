import type { PreviewContext, PreviewPlugin } from "@open-file-viewer/core";
import { t } from "../../i18n";
import { sandboxedHtml } from "./htmlPreviewPolicy";

/**
 * Renders an HTML file inside a sandboxed iframe.
 *
 * The viewer library has no HTML renderer: `textPlugin()` claims `text/html` and shows the file as
 * syntax-highlighted source, capped at `MAX_RENDER_CHARS` (600 000 characters). For the file this
 * exists for — a self-contained report whose bytes are mostly inline images — that cap cuts the
 * document in half and shows it as code, which is not a preview of anything. Rendering is the only
 * useful view, so this plugin claims `html`/`htm` first.
 *
 * It is a plugin rather than a bespoke component so the viewer's own toolbar, download and
 * error states keep working for this file type. Plugin order decides the winner and the first
 * matching plugin renders, so `FilePreview` must register this before `textPlugin()`.
 *
 * The isolation is not invented here: it is the same shape the library uses for the untrusted
 * documents it already renders (an iframe with an explicit `sandbox`, see `emailPlugin`), with the
 * policy in `htmlPreviewPolicy` closing the network side.
 */
export function htmlPlugin(): PreviewPlugin {
  return {
    name: "html",
    match(file) {
      return file.extension === "html" || file.extension === "htm";
    },
    async render(ctx: PreviewContext) {
      ctx.setLoading(true);
      try {
        const html = await readHtml(ctx);
        const frame = document.createElement("iframe");
        frame.setAttribute("sandbox", "allow-scripts");
        frame.setAttribute("referrerpolicy", "no-referrer");
        frame.setAttribute("title", ctx.file.name);
        // An inline size is deliberate: the plugin owns one element inside the viewer's viewport,
        // and the frame has to fill it rather than fall back to the 300×150 default.
        frame.style.cssText = "display:block;width:100%;height:100%;border:0;background:#fff";
        frame.srcdoc = sandboxedHtml(html);
        ctx.viewport.append(frame);
        return {
          // Zoom, rotate and search act on the document this plugin drew, not on a copy the
          // viewer can transform; reporting them unsupported greys the buttons out instead of
          // leaving them clickable and inert.
          canCommand: () => false,
          command: () => false,
          destroy: () => frame.remove(),
        };
      } catch (error) {
        ctx.setError(t("preview.htmlFailed"));
        throw error;
      } finally {
        ctx.setLoading(false);
      }
    },
  };
}

/** The file's bytes as text; a Blob source is already in memory, a URL is one request. */
async function readHtml(ctx: PreviewContext): Promise<string> {
  const blob = ctx.file.blob ?? (await readSource(ctx));
  return blob.text();
}

async function readSource(ctx: PreviewContext): Promise<Blob> {
  const source = ctx.file.source;
  if (typeof source === "string") {
    // The URL is a short-lived read grant (or a blob: URL the caller already built), not a
    // session-protected origin, so no credentials travel with the request.
    const response = await fetch(source, { signal: ctx.signal, credentials: "omit" });
    if (!response.ok) throw new Error(`HTML preview read failed: HTTP ${response.status}`);
    return response.blob();
  }
  if (source instanceof Blob) return source;
  return new Blob([source], { type: "text/html" });
}
