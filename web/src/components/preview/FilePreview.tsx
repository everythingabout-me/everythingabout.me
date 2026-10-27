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
 * 预览器主体，整个文件只通过 `React.lazy` 进入：首屏不下载这一块。
 *
 * 插件只建一次——每次渲染都 new 一份会重建预览实例，缩放和滚动位置都会丢。
 * 这一层只启用六个常用家族（图片 / PDF / Office / 文本 / 音视频）；CAD、3D、GIS
 * 要额外拖 mlightcad、three、leaflet，对个人文件库不值得，需要时再加。
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
  // 预签名地址只负责 302；字节由浏览器直连 COS，服务端不中转。
  const url = api.downloadHref(path);

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") onClose();
    };
    // 弹层下面还有一整页列表，滚动要锁住，否则滚轮会把背后的表格带着走。
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
