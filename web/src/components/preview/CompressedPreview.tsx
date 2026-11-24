import { useEffect, useMemo, useState } from "react";
import { FileViewer } from "@open-file-viewer/react";
import "@open-file-viewer/core/style.css";
import { api, isNativeAbsent } from "../../api/client";
import { locale, t } from "../../i18n";
import { formatSize } from "../../pages/sizeFormat";
import { DownloadIcon } from "../icons";
import { StorageNotice } from "../StorageNotice";
import type { Bytes } from "./bytes";
import { CompressionError, MAX_COMPRESSED_BYTES, inflateChain, type CompressionErrorKind } from "./compression";
import { previewPlugins } from "./plugins";
import { isArchiveFile } from "./supported";
import { MemoryByteSource } from "./zipArchive";
import { ZipBrowser } from "./ZipPreview";
import styles from "./CompressedPreview.module.css";

/**
 * A compressed file's contents, inflated in the browser and rendered as the file it holds.
 *
 * A single-stream compression has no listing to show, so the answer to "what is this" is the
 * decompressed file — `session.v4.jsonl` out of `session.v4.jsonl.zstd` — rendered by the same
 * plugin chain (and, for an inner ZIP, the same archive browser) as if it had been uploaded plain.
 * The caption names that inner file, because the page above it names the compressed one and the
 * difference is the whole point of the preview.
 *
 * The bytes come from the read grant the detail page would have used anyway; nothing is extracted
 * to the server, so the storage driver and the grant's guarantees are untouched (see
 * docs/design/compressed-preview.md).
 */

type Load =
  | { kind: "reading" }
  | { kind: "ready"; name: string; bytes: Bytes }
  /** `limit` is set only where the number is this file's: the fetch cap, not a decoder bound. */
  | { kind: "failed"; reason: CompressionErrorKind | "read_failed" | "missing"; limit?: number };

export default function CompressedPreview({ path, name }: { path: string; name: string }) {
  const [load, setLoad] = useState<Load>({ kind: "reading" });
  const [downloadError, setDownloadError] = useState<string | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    setLoad({ kind: "reading" });
    setDownloadError(null);
    void (async () => {
      try {
        const grant = await api.downloadGrant(path, "attachment", controller.signal);
        if (controller.signal.aborted) return;
        // Inflation is single-stream: the whole compressed object has to exist before the first
        // byte comes out, so an object past the fetch cap answers before the download starts.
        if (grant.size > MAX_COMPRESSED_BYTES) {
          setLoad({ kind: "failed", reason: "too_large", limit: MAX_COMPRESSED_BYTES });
          return;
        }
        const response = await fetch(grant.url, grant.kind === "bearer"
          ? { headers: grant.headers ?? {}, credentials: "omit", signal: controller.signal }
          : { credentials: "same-origin", signal: controller.signal });
        if (!response.ok) throw new Error(`The read grant answered HTTP ${response.status}.`);
        const bytes = new Uint8Array(await response.arrayBuffer());
        const content = await inflateChain(bytes, name, { signal: controller.signal });
        if (controller.signal.aborted) return;
        setLoad({ kind: "ready", name: content.name, bytes: content.bytes });
      } catch (error) {
        if (controller.signal.aborted) return;
        if (error instanceof CompressionError) setLoad({ kind: "failed", reason: error.kind });
        else setLoad({ kind: "failed", reason: isNativeAbsent(error) ? "missing" : "read_failed" });
      }
    })();
    return () => controller.abort();
  }, [path, name]);

  // The inflated bytes are the only thing that should re-render the viewer: handing it a fresh
  // Blob or archive source on an unrelated render would rebuild the preview and lose its scroll.
  const content = load.kind === "ready" ? load : null;
  const blob = useMemo(() => (content ? new Blob([content.bytes]) : null), [content]);
  const archive = useMemo(
    () => (content && isArchiveFile(content.name) ? new MemoryByteSource(content.bytes) : null),
    [content],
  );

  const download = async () => {
    setDownloadError(null);
    try {
      await api.download(path);
    } catch (error) {
      setDownloadError(error instanceof Error ? error.message : t("error.unknown"));
    }
  };

  if (load.kind === "failed") {
    return (
      <div className={styles.compressed}>
        <div className={styles.notice} role="alert">
          {load.reason === "missing" ? (
            <StorageNotice missingContent />
          ) : (
            <>
              <strong>{failureTitle(load.reason)}</strong>
              {load.limit !== undefined ? (
                <span className={styles.detail}>
                  {t("compressed.limit", { size: formatSize(load.limit) })}
                </span>
              ) : null}
              <button className={styles.download} type="button" onClick={() => void download()}>
                <DownloadIcon size={14} />
                {t("file.download")}
              </button>
              {downloadError ? <span className={styles.downloadError} role="alert">{downloadError}</span> : null}
            </>
          )}
        </div>
      </div>
    );
  }

  if (!content) {
    return (
      <div className={styles.compressed}>
        <p className={styles.notice} role="status">{t("compressed.reading")}</p>
      </div>
    );
  }

  return (
    <div className={styles.compressed}>
      <header className={styles.caption}>
        <span className={styles.name} title={content.name}>{content.name}</span>
        <span className={styles.size}>{t("compressed.size", { size: formatSize(content.bytes.length) })}</span>
      </header>
      <div className={styles.body}>
        {archive ? (
          <ZipBrowser label={content.name} source={archive} />
        ) : blob ? (
          <FileViewer
            file={blob}
            fileName={content.name}
            width="100%"
            height="100%"
            fit="contain"
            toolbar
            theme="auto"
            locale={locale === "zh" ? "zh-CN" : "en-US"}
            plugins={previewPlugins}
          />
        ) : null}
      </div>
    </div>
  );
}

/**
 * The reason a compressed file did not open, as a sentence.
 *
 * The decoder's own diagnostics stay out of the panel: they are English, and this product's copy
 * is not. What a person needs is which of the few honest outcomes happened, and the download
 * button is offered next to all of them — the original is untouched either way.
 */
function failureTitle(reason: CompressionErrorKind | "read_failed"): string {
  switch (reason) {
    case "too_large":
      return t("compressed.tooLarge");
    case "not_compressed":
      return t("compressed.notCompressed");
    case "damaged":
      return t("compressed.damaged");
    case "unsupported":
      return t("compressed.unsupported");
    case "nested":
      return t("compressed.nested");
    case "read_failed":
      return t("compressed.failed");
  }
}
