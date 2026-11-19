/**
 * URL helpers for the files route.
 *
 * A drive address is /<handle>/<logical path>: the account's handle is the first segment,
 * so a file's address is something a person can type, paste into a note and recognise as
 * theirs. The handle is not identity (ids are) and not a credential (the session is); it
 * is only the name in the address, which is why it lives in this module and nowhere in
 * the file API.
 *
 * The base is module state because every caller already links relative to "the drive" and
 * threading a base through eight link builders would let one of them forget. It is set by
 * each authenticated route loader from the signed-in account, so it is never stale across
 * a rename or a different account.
 */
let driveBase = "";

/** setDriveBase names the signed-in account's address prefix; pass null before a claim. */
export function setDriveBase(username: string | null | undefined): void {
  driveBase = username ? `/${username}` : "";
}

export function filesHref(path: string): string {
  const relative = path.startsWith("/") ? path.slice(1) : path;
  if (!relative) return driveBase || "/";
  // Before a handle is known (no signed-in account yet) the legacy /files route is the
  // safe spelling: it redirects once a loader has set the base, and it never invents a
  // first segment that the router would read as someone's handle.
  const prefix = driveBase || "/files";
  return `${prefix}/${relative.split("/").map(encodeURIComponent).join("/")}`;
}

/**
 * Read the logical path out of the current location. /files/... is accepted because
 * bookmarks predate the handle addresses and the cost of one string check is nil.
 */
export function filesRoutePath(pathname: string): string {
  let rest = pathname;
  if (driveBase !== "" && (rest === driveBase || rest.startsWith(`${driveBase}/`))) {
    rest = rest.slice(driveBase.length);
  } else if (rest === "/files" || rest === "/files/") {
    rest = "";
  } else if (rest.startsWith("/files/")) {
    rest = rest.slice("/files".length);
  }
  if (rest === "" || rest === "/") return "";
  return rest
    .slice(1)
    .split("/")
    .map(decodeURIComponent)
    .join("/");
}
