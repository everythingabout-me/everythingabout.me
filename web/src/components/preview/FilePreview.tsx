import { useEffect, useRef } from "react";
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
import { api } from "../../api/client";
import { locale, t } from "../../i18n";
import { CloseIcon } from "../icons";
import styles from "./FilePreview.module.css";

/**
 * The previewer proper. The whole file only enters through `React.lazy`: the first screen never
 * downloads this chunk.
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

export default function FilePreview({
  path,
  name,
  onClose,
}: {
  path: string;
  name: string;
  onClose: () => void;
}) {
  const dialog = useRef<HTMLDivElement>(null);
  // The presigned URL only handles the 302; the browser talks to COS directly, never through us.
  const url = api.downloadHref(path);

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") onClose();
    };
    // A full listing page sits under the overlay: lock scrolling or the wheel drags it along.
    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    dialog.current?.focus();
    window.addEventListener("keydown", onKeyDown);
    return () => {
      window.removeEventListener("keydown", onKeyDown);
      document.body.style.overflow = previousOverflow;
    };
  }, [onClose]);

  return (
    <div
      ref={dialog}
      className={styles.overlay}
      role="dialog"
      aria-modal="true"
      aria-label={name}
      tabIndex={-1}
    >
      <div className={styles.shell}>
        <header className={styles.header}>
          <span className={styles.title} title={name}>
            {name}
          </span>
          <a className={styles.action} href={url} target="_blank" rel="noreferrer">
            {t("preview.open")}
          </a>
          <button
            className={styles.closeBtn}
            onClick={onClose}
            aria-label={t("common.close")}
            title={t("common.close")}
          >
            <CloseIcon size={14} />
          </button>
        </header>

        <div className={styles.viewport}>
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
      </div>
    </div>
  );
}
