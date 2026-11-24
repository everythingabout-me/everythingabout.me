/**
 * URL helpers for the files route.
 *
 * A drive address is `/files/<logical path>` and nothing else: no account name, because the
 * account is already named by the session. Every signed-in person therefore sees their own
 * tree at the same addresses — the paths of two accounts collide by design, and that
 * collision is the truth we want on screen: these are two separate private trees, not pages
 * of one public site. An address that spelled the account out would read like a profile
 * (`/someone/…`), which is exactly the impression this product must not give.
 *
 * The fixed prefix exists so the tree cannot shadow the app's own routes: without it a folder
 * named `settings` would make the settings page unreachable, and a top-level file named
 * `README.md` would collide with the SPA's missing-asset rule. With it, `/files/…` is files
 * and everything else is the app.
 */
const DRIVE_PREFIX = "/files";

export function filesHref(path: string): string {
  const relative = path.startsWith("/") ? path.slice(1) : path;
  if (!relative) return DRIVE_PREFIX;
  return `${DRIVE_PREFIX}/${relative.split("/").map(encodeURIComponent).join("/")}`;
}

/** Read the logical path out of the current location (`/files/…`), decoded per segment. */
export function filesRoutePath(pathname: string): string {
  let rest = pathname;
  if (rest === DRIVE_PREFIX || rest === `${DRIVE_PREFIX}/`) {
    rest = "";
  } else if (rest.startsWith(`${DRIVE_PREFIX}/`)) {
    rest = rest.slice(DRIVE_PREFIX.length);
  }
  if (rest === "" || rest === "/") return "";
  return rest
    .slice(1)
    .split("/")
    .map(decodeURIComponent)
    .join("/");
}
