/**
 * The sandbox policy for a previewed HTML file.
 *
 * Two separate boundaries, because a `.html` file in a personal drive is user content that may
 * legitimately contain script — a self-contained report with charts is the normal case, and
 * rendering it with scripts disabled shows blank charts.
 *
 * - **The iframe** (`sandbox="allow-scripts"`, no `allow-same-origin`) gives the document an opaque
 *   origin: no cookies, no storage, no handle on this app's DOM or API. That is what keeps a
 *   hostile upload from acting as the signed-in user.
 * - **This CSP** is what keeps the same document from talking to the network: `connect-src 'none'`
 *   blocks fetch/XHR/beacon, and images, media and fonts are limited to `data:`/`blob:`. Without it
 *   the sandbox would still let the file exfiltrate its own contents to any server.
 *
 * Remote scripts stay blocked on purpose. A report that pulls a library from a CDN renders without
 * it: that is a visible gap the user can fix by embedding the library, while the alternative —
 * trusting a third-party origin with code execution inside their drive — is not a gap we can make
 * visible. `htmlCardPolicy` is the stricter sibling used for app-supplied event cards, where the
 * HTML is not the user's and no script is needed at all.
 */
export function fileHtmlPolicy(): string {
  return [
    "default-src 'none'",
    "script-src 'unsafe-inline' 'unsafe-eval'",
    "style-src 'unsafe-inline'",
    "img-src data: blob:",
    "font-src data:",
    "media-src data: blob:",
    "connect-src 'none'",
    "form-action 'none'",
    "base-uri 'none'",
  ].join("; ");
}

/**
 * The document handed to the iframe.
 *
 * The policy has to travel inside the document: a `srcdoc` frame receives no response headers, so
 * a meta element is the only place a file preview can attach a CSP. It is inserted after the
 * doctype and nowhere else — a meta before `<!doctype html>` drops the document into quirks mode
 * and breaks the layout of a report written for standards mode. The rest of the bytes are left
 * untouched: the preview shows the user their file, not a rewritten copy of it.
 */
export function sandboxedHtml(html: string): string {
  const head =
    '<meta charset="utf-8">' +
    `<meta http-equiv="Content-Security-Policy" content="${fileHtmlPolicy()}">`;
  const doctype = /^\s*<!doctype[^>]*>/i.exec(html);
  if (!doctype) return head + html;
  const after = doctype.index + doctype[0].length;
  return html.slice(0, after) + head + html.slice(after);
}
