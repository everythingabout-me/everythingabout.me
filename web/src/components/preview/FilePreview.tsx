import { useEffect, useState } from "react";
import { FileViewer } from "@open-file-viewer/react";
import "@open-file-viewer/core/style.css";
import { acquirePrefetchedPreview, api, isNativeAbsent } from "../../api/client";
import { locale, t } from "../../i18n";
import { StorageNotice } from "../StorageNotice";
import CompressedPreview from "./CompressedPreview";
import { compressionKind } from "./compression";
import { previewPlugins } from "./plugins";
import { isArchiveFile } from "./supported";
import ZipPreview from "./ZipPreview";
import styles from "./FilePreview.module.css";

/**
 * The inline preview on a file's detail page. The whole file only enters through `React.lazy`:
 * the first screen never downloads this chunk.
 *
 * A ZIP is not rendered by the viewer but by `ZipPreview`, which needs the archive's directory
 * before it has a file to hand to a plugin — and, unlike every other format, must not download
 * the whole object to work out what is inside it. The plugin chain is shared, so an entry
 * rendered inside the archive is the same preview as the same file on its own.
 *
 * A compressed file (`.gz`, `.zst`) is the one case where the viewer has nothing to show until
 * something is undone: `CompressedPreview` inflates the stream and previews the file it holds,
 * under that file's own name. It is not the archive path because there is no listing — the
 * content *is* the file that was compressed.
 */
export default function FilePreview({ path, name, mediaType }: { path: string; name: string; mediaType: string | null }) {
  if (isArchiveFile(name)) {
    return (
      <div className={styles.viewer}>
        <ZipPreview path={path} name={name} />
      </div>
    );
  }
  if (compressionKind(name)) {
    return (
      <div className={styles.viewer}>
        <CompressedPreview path={path} name={name} />
      </div>
    );
  }
  return <MediaPreview path={path} name={name} mediaType={mediaType} />;
}

function MediaPreview({ path, name, mediaType }: { path: string; name: string; mediaType: string | null }) {
  // Render fetched bytes through plugins; only safe media receives inline disposition.
  const safeInline = /^(image\/(jpeg|png|gif|webp|avif)|audio\/|video\/)/.test(mediaType ?? "");
  const [url, setURL] = useState<string | null>(null);
  // A cross-origin media element never exposes its status, so a file storage no longer
  // has would look like an eternal spinner. The grant itself now fails, and that answer
  // decides between "moved outside the app" and an ordinary preview failure.
  const [failure, setFailure] = useState<"missing" | "error" | null>(null);
  useEffect(() => {
    setFailure(null);
    // The page prepared this file's bytes while the previous one was open, so a page turn
    // renders from memory instead of asking for a grant and downloading the object again.
    // The cache keeps owning that URL; this render only holds it until it unmounts.
    const prepared = acquirePrefetchedPreview(path);
    if (prepared) {
      setURL(prepared.url);
      return prepared.release;
    }
    setURL(null);
    const controller = new AbortController();
    let created: string | null = null;
    void api.downloadURL(path, safeInline ? "inline" : "attachment", controller.signal).then((next) => {
      created = next;
      if (!controller.signal.aborted) setURL(next);
    }).catch((error: unknown) => {
      if (controller.signal.aborted) return;
      setFailure(isNativeAbsent(error) ? "missing" : "error");
    });
    return () => {
      controller.abort();
      if (created?.startsWith("blob:")) URL.revokeObjectURL(created);
    };
  }, [path, safeInline]);

  if (!url) {
    if (failure === "missing") {
      return (
        <div className={styles.viewer}>
          <div className={styles.notice}>
            <StorageNotice missingContent />
          </div>
        </div>
      );
    }
    if (failure === "error") {
      return (
        <div className={styles.viewer}>
          <p className={styles.failure} role="alert">{t("preview.failed")}</p>
        </div>
      );
    }
    return <div className={styles.viewer} aria-busy="true" />;
  }
  return (
    <div className={styles.viewer}>
      <FileViewer
        file={url}
        fileName={name}
        width="100%"
        height="100%"
        fit="contain"
        toolbar
        theme="auto"
        locale={locale === "zh" ? "zh-CN" : "en-US"}
        plugins={previewPlugins}
      />
    </div>
  );
}
