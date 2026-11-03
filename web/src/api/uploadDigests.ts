/** Per-part checksums for signed COS ingress; loaded only when that transport is used. */
export interface UploadDigests {
  sha256: string;
  md5: string;
}

const cached = new WeakMap<Blob, Promise<UploadDigests>>();

export function uploadDigests(blob: Blob): Promise<UploadDigests> {
  const previous = cached.get(blob);
  if (previous) return previous;
  const pending = (async () => {
    const [{ default: SparkMD5 }, bytes] = await Promise.all([import("spark-md5"), blob.arrayBuffer()]);
    const [sha, md5] = await Promise.all([
      crypto.subtle.digest("SHA-256", bytes),
      Promise.resolve(SparkMD5.ArrayBuffer.hash(bytes, true)),
    ]);
    return {
      sha256: [...new Uint8Array(sha)].map((byte) => byte.toString(16).padStart(2, "0")).join(""),
      md5: btoa(md5),
    };
  })();
  cached.set(blob, pending);
  pending.catch(() => { cached.delete(blob); });
  return pending;
}
