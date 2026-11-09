import { useEffect, useState } from "react";
import {
  audioPlugin,
  imagePlugin,
  officePlugin,
  pdfPlugin,
  textPlugin,
  videoPlugin,
} from "@open-file-viewer/core";
import { FileViewer } from "@open-file-viewer/react";
import "@open-file-viewer/core/style.css";
import pdfLegacyWorkerSrc from "pdfjs-dist/legacy/build/pdf.worker.mjs?url";
import pdfWorkerSrc from "pdfjs-dist/build/pdf.worker.mjs?url";
import { api, isNativeAbsent } from "../../api/client";
import { locale, t } from "../../i18n";
import { StorageNotice } from "../StorageNotice";
import styles from "./FilePreview.module.css";

/**
 * The inline preview on a file's detail page. The whole file only enters through `React.lazy`:
 * the first screen never downloads this chunk.
 *
 * Plugins are built once — re-creating them on every render rebuilds the viewer instance and
 * loses zoom and scroll position. This layer enables only the six common families (images / PDF /
 * Office / text / audio and video); CAD, 3D and GIS would drag in mlightcad, three and leaflet,
 * which is not worth it for a personal file library. Add them when needed.
 */
const plugins = [
  imagePlugin(),
  textPlugin(),
  pdfPlugin({ workerSrc: pdfWorkerSrc, legacyWorkerSrc: pdfLegacyWorkerSrc }),
  officePlugin(),
  videoPlugin(),
  audioPlugin(),
];

export default function FilePreview({ path, name, mediaType }: { path: string; name: string; mediaType: string | null }) {
  // Render fetched bytes through plugins; only safe media receives inline disposition.
  const safeInline = /^(image\/(jpeg|png|gif|webp|avif)|audio\/|video\/)/.test(mediaType ?? "");
  const [url, setURL] = useState<string | null>(null);
  // A cross-origin media element never exposes its status, so a file storage no longer
  // has would look like an eternal spinner. The grant itself now fails, and that answer
  // decides between "moved outside the app" and an ordinary preview failure.
  const [failure, setFailure] = useState<"missing" | "error" | null>(null);
  useEffect(() => {
    setURL(null);
    setFailure(null);
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
        plugins={plugins}
      />
    </div>
  );
}
