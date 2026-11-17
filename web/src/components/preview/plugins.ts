import {
  audioPlugin,
  imagePlugin,
  officePlugin,
  pdfPlugin,
  textPlugin,
  videoPlugin,
} from "@open-file-viewer/core";
import pdfLegacyWorkerSrc from "pdfjs-dist/legacy/build/pdf.worker.mjs?url";
import pdfWorkerSrc from "pdfjs-dist/build/pdf.worker.mjs?url";
import { heicPlugin } from "./heicPlugin";
import { htmlPlugin } from "./htmlPlugin";

/**
 * The renderers a file may claim, in match order.
 *
 * This layer enables only the six common families (images / PDF / Office / text / audio and
 * video) plus HTML; CAD, 3D and GIS would drag in mlightcad, three and leaflet, which is not
 * worth it for a personal file library. Add them when needed.
 *
 * Order is a contract: the first matching plugin renders. `textPlugin` also matches `text/html`
 * and `imagePlugin` also claims `heic`, so both replacements have to come before the upstream
 * plugins they replace, or the file silently falls back to source / to a decoder that cannot read
 * iPhone photos.
 *
 * Built once — re-creating them on every render rebuilds the viewer instance and loses zoom and
 * scroll position. Zip previews render an extracted entry through this same list, so a file
 * inside an archive looks exactly like the same file uploaded on its own.
 */
export const previewPlugins = [
  htmlPlugin(),
  heicPlugin(),
  imagePlugin(),
  textPlugin(),
  pdfPlugin({ workerSrc: pdfWorkerSrc, legacyWorkerSrc: pdfLegacyWorkerSrc }),
  officePlugin(),
  videoPlugin(),
  audioPlugin(),
];
