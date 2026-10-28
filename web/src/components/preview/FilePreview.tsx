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
import { locale } from "../../i18n";
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

export default function FilePreview({ path, name }: { path: string; name: string }) {
  // The presigned URL only handles the 302; the browser talks to COS directly, never through us.
  const url = api.downloadHref(path);

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
