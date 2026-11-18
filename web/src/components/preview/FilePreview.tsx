import { useCallback, useEffect, useState } from "react";
import { FileViewer } from "@open-file-viewer/react";
import "@open-file-viewer/core/style.css";
import { api, isNativeAbsent } from "../../api/client";
import { locale, t } from "../../i18n";
import { StorageNotice } from "../StorageNotice";
import { pdfTailLooksEncrypted } from "./pdfEncryption";
import PdfPasswordPreview from "./PdfPasswordPreview";
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
 */
export default function FilePreview({ path, name, mediaType }: { path: string; name: string; mediaType: string | null }) {
  if (isArchiveFile(name)) {
    return (
      <div className={styles.viewer}>
        <ZipPreview path={path} name={name} />
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
  // A PDF's bytes can carry a password. That is not a failure, but the bundled viewer answers it
  // with a download card, so the file is mounted here instead — see `pdfEncryption` for how the
  // question is answered without parsing the document twice.
  const isPdf = mediaType === "application/pdf" || /\.pdf$/i.test(name);
  const [locked, setLocked] = useState<boolean | null>(isPdf ? null : false);
  useEffect(() => {
    setURL(null);
    setFailure(null);
    setLocked(isPdf ? null : false);
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
  }, [path, safeInline, isPdf]);

  useEffect(() => {
    if (!url || !isPdf) return;
    const controller = new AbortController();
    void pdfTailLooksEncrypted(url, controller.signal).then(
      (encrypted) => {
        if (!controller.signal.aborted) setLocked(encrypted);
      },
      () => {
        // A tail that cannot be read leaves the ordinary viewer in place: it is the better answer
        // for every file that is not locked, and its own card is unchanged for the rest.
        if (!controller.signal.aborted) setLocked(false);
      },
    );
    return () => controller.abort();
  }, [url, isPdf]);

  const openWithoutPassword = useCallback(() => setLocked(false), []);

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
  if (locked) {
    return (
      <div className={styles.viewer}>
        <PdfPasswordPreview url={url} onOpenWithoutPassword={openWithoutPassword} />
      </div>
    );
  }
  if (locked === null) return <div className={styles.viewer} aria-busy="true" />;
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
