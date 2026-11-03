/** Coordinates cooperating tabs on this origin; it does not fence other clients or devices. */
export async function claimUploadPath(owner: string, path: string, signal: AbortSignal): Promise<(() => void) | null> {
  const aborted = () => new DOMException("Upload ownership canceled", "AbortError");
  if (signal.aborted) throw aborted();
  const locks = typeof navigator === "undefined" ? undefined : navigator.locks;
  if (typeof locks?.request !== "function") throw new Error("Upload path ownership requires Web Locks");

  const normalized = path.normalize("NFC").replace(/^\/+|\/+$/g, "");
  const name = "eam.upload-path.v1:" + JSON.stringify([owner, "/" + normalized]);
  const release = await new Promise<(() => void) | null>((resolve, reject) => {
    let unlock: (() => void) | undefined;
    const release = () => {
      signal.removeEventListener("abort", abort);
      unlock?.();
      unlock = undefined;
    };
    const abort = () => {
      release();
      reject(aborted());
    };
    const failed = (error: unknown) => {
      release();
      reject(error instanceof Error ? error : new Error("Upload path ownership request failed", { cause: error }));
    };
    signal.addEventListener("abort", abort, { once: true });
    try {
      // Web Locks forbids a native signal with ifAvailable; cancellation is handled above.
      const request = locks.request(name, { mode: "exclusive", ifAvailable: true }, (lock) => {
        // A canceled request can still have a callback awaiting delivery. Never retain that lock.
        if (signal.aborted) { abort(); return; }
        if (!lock) { release(); resolve(null); return; }
        const held = new Promise<void>((resolve) => { unlock = resolve; });
        resolve(release);
        return held;
      });
      void request.catch(failed);
    } catch (error) {
      failed(error);
    }
  });
  // Abort may follow the grant before this async function can deliver its release handle.
  if (signal.aborted) {
    release?.();
    throw aborted();
  }
  return release;
}
