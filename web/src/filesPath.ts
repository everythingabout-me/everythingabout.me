/**
 * URL helpers for the files route.
 *
 * A logical path is the URL: /files/Photos/2024/a.jpg. Each segment is percent-encoded on its own,
 * so `/` keeps separating segments and spaces or CJK characters stay readable in the address bar.
 */
export function filesHref(path: string): string {
  if (!path) return "/files";
  return `/files/${path.split("/").map(encodeURIComponent).join("/")}`;
}
