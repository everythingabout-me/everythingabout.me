import { useEffect, useRef, useState, type FormEvent } from "react";
import * as pdfjs from "pdfjs-dist";
import type { PDFDocumentLoadingTask, PDFDocumentProxy } from "pdfjs-dist";
import pdfWorkerSrc from "pdfjs-dist/build/pdf.worker.mjs?url";
import { t } from "../../i18n";
import styles from "./PdfPasswordPreview.module.css";

pdfjs.GlobalWorkerOptions.workerSrc = pdfWorkerSrc;

/**
 * A PDF whose bytes are locked, previewed here instead of being refused.
 *
 * The bundled viewer has no field to type a password into — it hands pdfjs a URL, and pdfjs answers
 * an encrypted document with a `PasswordException` that the viewer turns into a download card. This
 * component exists because that answer is not a dead end: pdfjs decrypts a password-protected file
 * in memory once it is given the password, so the file never has to be re-uploaded, copied, or sent
 * to the server. Only the reading of the bytes and the password the reader types ever exist here.
 *
 * A PDF that is encrypted but has an empty user password (printing or copying merely restricted)
 * opens on the first attempt and is handed back to the ordinary viewer, which has the toolbar this
 * one deliberately does not.
 */
export default function PdfPasswordPreview({
  url,
  onOpenWithoutPassword,
}: {
  url: string;
  /** The document needs no password; let the caller mount the viewer with the full toolbar. */
  onOpenWithoutPassword: () => void;
}) {
  const [phase, setPhase] = useState<"checking" | "locked" | "opening" | "ready" | "failed">("checking");
  const [document, setDocument] = useState<PDFDocumentProxy | null>(null);
  const [password, setPassword] = useState("");
  const [wrong, setWrong] = useState(false);
  const taskRef = useRef<PDFDocumentLoadingTask | null>(null);

  useEffect(() => {
    let cancelled = false;
    setPhase("checking");
    setDocument(null);
    setWrong(false);
    setPassword("");
    // The empty password is not a guess: it is the user password of a file that only forbids
    // printing or copying, and pdfjs needs no help with those.
    const task = openPdf(url, "");
    taskRef.current = task;
    task.promise.then(
      () => {
        if (!cancelled) onOpenWithoutPassword();
      },
      (error: unknown) => {
        if (cancelled) return;
        setPhase(isPasswordError(error) ? "locked" : "failed");
      },
    );
    return () => {
      cancelled = true;
      void task.destroy();
    };
  }, [url, onOpenWithoutPassword]);

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!password || phase === "opening") return;
    setPhase("opening");
    setWrong(false);
    const task = openPdf(url, password);
    taskRef.current = task;
    try {
      const opened = await task.promise;
      setDocument(opened);
      setPhase("ready");
    } catch (error) {
      if (isPasswordError(error)) {
        setPhase("locked");
        setWrong(true);
        return;
      }
      setPhase("failed");
    }
  }

  if (phase === "ready" && document) return <PdfPages document={document} />;
  if (phase === "checking") return <div className={styles.viewer} aria-busy="true" />;

  const failed = phase === "failed";
  return (
    <div className={styles.viewer}>
      <form className={styles.card} onSubmit={submit}>
        <h3 className={styles.title}>{failed ? t("pdf.unlockFailed") : t("pdf.passwordTitle")}</h3>
        <p className={styles.text}>{failed ? t("pdf.unlockFailedText") : t("pdf.passwordText")}</p>
        {!failed && (
          <>
            <label className={styles.label} htmlFor="pdf-password">
              {t("pdf.passwordLabel")}
            </label>
            <input
              id="pdf-password"
              className={styles.input}
              type="password"
              value={password}
              placeholder={t("pdf.passwordPlaceholder")}
              autoComplete="off"
              autoFocus
              spellCheck={false}
              aria-invalid={wrong}
              onChange={(event) => {
                setPassword(event.target.value);
                setWrong(false);
              }}
            />
            {wrong && (
              <p className={styles.error} role="alert">
                {t("pdf.wrongPassword")}
              </p>
            )}
            <div className={styles.actions}>
              <button className={styles.open} type="submit" disabled={!password || phase === "opening"}>
                {phase === "opening" ? t("pdf.opening") : t("pdf.open")}
              </button>
              <a className={styles.download} href={url} download>
                {t("pdf.downloadOriginal")}
              </a>
            </div>
          </>
        )}
        {failed && (
          <a className={styles.download} href={url} download>
            {t("pdf.downloadOriginal")}
          </a>
        )}
      </form>
    </div>
  );
}

/** The pages of an unlocked document: one canvas per page, rendered as it scrolls into reach. */
function PdfPages({ document }: { document: PDFDocumentProxy }) {
  const sheetRef = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(0);
  const [zoom, setZoom] = useState(1);

  useEffect(() => {
    const node = sheetRef.current;
    if (!node) return;
    const update = () => setWidth(node.clientWidth);
    update();
    const observer = new ResizeObserver(update);
    observer.observe(node);
    return () => observer.disconnect();
  }, []);

  const pageWidth = width > 0 ? Math.round(width * zoom) : 0;
  return (
    <div className={styles.document}>
      <div className={styles.toolbar}>
        <button
          type="button"
          className={styles.tool}
          aria-label={t("pdf.zoomOut")}
          disabled={zoom <= 0.5}
          onClick={() => setZoom((value) => Math.max(0.5, value - 0.25))}
        >
          −
        </button>
        <span className={styles.zoom}>{Math.round(zoom * 100)}%</span>
        <button
          type="button"
          className={styles.tool}
          aria-label={t("pdf.zoomIn")}
          disabled={zoom >= 4}
          onClick={() => setZoom((value) => Math.min(4, value + 0.25))}
        >
          +
        </button>
        <button type="button" className={styles.tool} onClick={() => setZoom(1)}>
          {t("pdf.fitWidth")}
        </button>
        <span className={styles.count}>{t("pdf.pageOf", { total: document.numPages })}</span>
      </div>
      <div className={styles.scroller}>
        <div className={styles.sheet} ref={sheetRef}>
          {pageWidth > 0 &&
            Array.from({ length: document.numPages }, (_, index) => (
              <PdfPage key={index} document={document} number={index + 1} width={pageWidth} />
            ))}
        </div>
      </div>
    </div>
  );
}

function PdfPage({ document, number, width }: { document: PDFDocumentProxy; number: number; width: number }) {
  const holderRef = useRef<HTMLDivElement>(null);
  const canvasRef = useRef<HTMLCanvasElement>(null);
  // A4 proportions until the page reports its own, so the scrollbar does not jump as pages load.
  const [ratio, setRatio] = useState(1.294);
  const [visible, setVisible] = useState(false);

  useEffect(() => {
    const node = holderRef.current;
    if (!node) return;
    const observer = new IntersectionObserver(
      (entries) => {
        if (entries.some((entry) => entry.isIntersecting)) setVisible(true);
      },
      { rootMargin: "800px 0px" },
    );
    observer.observe(node);
    return () => observer.disconnect();
  }, []);

  useEffect(() => {
    if (!visible) return;
    let cancelled = false;
    let task: pdfjs.RenderTask | null = null;
    void (async () => {
      const page = await document.getPage(number);
      if (cancelled) return;
      const base = page.getViewport({ scale: 1 });
      const viewport = page.getViewport({ scale: width / base.width });
      const canvas = canvasRef.current;
      const context = canvas?.getContext("2d");
      if (!canvas || !context) return;
      const density = window.devicePixelRatio || 1;
      canvas.width = Math.floor(viewport.width * density);
      canvas.height = Math.floor(viewport.height * density);
      canvas.style.width = `${Math.floor(viewport.width)}px`;
      canvas.style.height = `${Math.floor(viewport.height)}px`;
      setRatio(viewport.height / viewport.width);
      task = page.render({
        canvasContext: context,
        viewport,
        transform: density === 1 ? undefined : [density, 0, 0, density, 0, 0],
      });
      await task.promise;
    })().catch(() => {
      // A page that fails to draw leaves its placeholder; the rest of the document stays readable.
    });
    return () => {
      cancelled = true;
      task?.cancel();
    };
  }, [document, number, width, visible]);

  return (
    <div ref={holderRef} className={styles.page} style={{ width, aspectRatio: `1 / ${ratio}` }}>
      <canvas ref={canvasRef} className={styles.canvas} />
    </div>
  );
}

function openPdf(url: string, password: string): PDFDocumentLoadingTask {
  return pdfjs.getDocument({
    url,
    password,
    isEvalSupported: false,
    // The same CDN defaults the bundled viewer uses: the app ships no CMaps or standard fonts, and
    // a CJK statement without its CMap renders as blank pages.
    cMapUrl: `https://cdn.jsdelivr.net/npm/pdfjs-dist@${pdfjs.version}/cmaps/`,
    cMapPacked: true,
    standardFontDataUrl: `https://cdn.jsdelivr.net/npm/pdfjs-dist@${pdfjs.version}/standard_fonts/`,
  });
}

function isPasswordError(error: unknown): boolean {
  return error instanceof Error && error.name === "PasswordException";
}
