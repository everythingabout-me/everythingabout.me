/**
 * URL helpers for the files route.
 *
 * A logical path is the URL: /files/Photos/2024/a.jpg. Each segment is percent-encoded on its own,
 * so `/` keeps separating segments and spaces or CJK characters stay readable in the address bar.
 */
export function filesHref(path: string): string {
  const relative = path.startsWith("/") ? path.slice(1) : path;
  if (!relative) return "/files";
  return `/files/${relative.split("/").map(encodeURIComponent).join("/")}`;
}

/** Read the raw location: router splat decoding can interpret a literal %2F twice. */
export function filesRoutePath(pathname: string): string {
  if (pathname === "/files" || pathname === "/files/") return "";
  return pathname.slice("/files/".length).split("/").map(decodeURIComponent).join("/");
}
