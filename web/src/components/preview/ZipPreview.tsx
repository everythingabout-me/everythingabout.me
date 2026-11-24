import { useEffect, useMemo, useRef, useState } from "react";
import { FileViewer } from "@open-file-viewer/react";
import "@open-file-viewer/core/style.css";
import { api } from "../../api/client";
import { filesText, locale, t } from "../../i18n";
import { formatSize } from "../../pages/sizeFormat";
import { DownloadIcon } from "../icons";
import type { Bytes } from "./bytes";
import { CompressionError, compressionKind, inflateChain, uncompressedName } from "./compression";
import { previewPlugins } from "./plugins";
import { canPreviewFile, isArchiveFile } from "./supported";
import {
  HttpByteSource,
  MAX_ENTRY_BYTES,
  MemoryByteSource,
  readZipDirectory,
  readZipEntry,
  ZipError,
  type ByteSource,
  type ZipEntry,
} from "./zipArchive";
import styles from "./ZipPreview.module.css";

/**
 * A ZIP archive's contents, browsable without downloading it.
 *
 * The listing comes from two ranged reads (see `zipArchive`), so opening a 58 MB archive costs a
 * few kilobytes. Selecting a file inflates just that entry and hands it to the same plugin chain
 * the detail page uses, which is what makes a file inside an archive look exactly like the same
 * file uploaded on its own — including HEIC photos and sandboxed HTML.
 *
 * A nested archive is drilled into rather than embedded: embedding would nest a sidebar inside a
 * sidebar, while a breadcrumb keeps one list on screen and makes the way back obvious.
 *
 * The two halves are separate components because a ZIP can also *be inside* something: a
 * compressed `.zip.gz` hands `ZipBrowser` bytes it already inflated in memory, and the browser
 * neither knows nor cares where they came from.
 */

/** Rows rendered at once. An archive with 50 000 entries stays responsive; the filter is how the
 *  rest is reached, and the footer says how many matched. */
const MAX_ROWS = 500;

interface Level {
  /** What the breadcrumb calls this archive. */
  label: string;
  source: ByteSource;
  entries: ZipEntry[];
}

interface Selection {
  entry: ZipEntry;
  /** The name the pane previews under: an entry's own name, or the inner name once inflated. */
  name: string;
  kind: "loading" | "ready" | "failed" | "unsupported" | "large";
  blob?: Blob;
  detail?: string;
}

/** Fetch the archive behind this path, then browse it. */
export default function ZipPreview({ path, name }: { path: string; name: string }) {
  const [source, setSource] = useState<ByteSource | null>(null);
  const [failure, setFailure] = useState<string | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    setSource(null);
    setFailure(null);
    void (async () => {
      try {
        const grant = await api.downloadGrant(path, "attachment", controller.signal);
        if (controller.signal.aborted) return;
        setSource(new HttpByteSource({
          url: grant.url,
          headers: grant.headers,
          // A bearer grant is not cookie-authenticated; a same-origin relay URL is.
          credentials: grant.kind === "bearer" ? "omit" : "same-origin",
          size: grant.size,
          acceptRanges: grant.acceptRanges,
        }));
      } catch (error) {
        if (controller.signal.aborted) return;
        setFailure(archiveFailure(error));
      }
    })();
    return () => controller.abort();
  }, [path]);

  if (failure) {
    return (
      <div className={styles.archive}>
        <div className={styles.notice} role="alert">{failure}</div>
      </div>
    );
  }
  if (!source) {
    return (
      <div className={styles.archive}>
        <div className={styles.notice} role="status">{t("zip.reading")}</div>
      </div>
    );
  }
  return <ZipBrowser label={name} source={source} />;
}

/** One archive's contents: the list, the filter, and the selected entry's preview. */
export function ZipBrowser({ label, source }: { label: string; source: ByteSource }) {
  const [levels, setLevels] = useState<Level[]>([]);
  const [reading, setReading] = useState(true);
  const [archiveError, setArchiveError] = useState<string | null>(null);
  const [filter, setFilter] = useState("");
  const [selection, setSelection] = useState<Selection | null>(null);
  // One controller per archive: the listing and every later extraction belong to the same file,
  // and leaving the page must cancel whichever of them is in flight.
  const abortRef = useRef<AbortController | null>(null);
  // A click that lands while an earlier extraction is still running wins; the stale result is
  // dropped instead of overwriting the newer selection.
  const requestRef = useRef(0);

  useEffect(() => {
    const controller = new AbortController();
    abortRef.current = controller;
    requestRef.current += 1;
    setLevels([]);
    setReading(true);
    setArchiveError(null);
    setSelection(null);
    setFilter("");
    void (async () => {
      try {
        const directory = await readZipDirectory(source, controller.signal);
        if (controller.signal.aborted) return;
        setLevels([{ label, source, entries: directory.entries }]);
        setReading(false);
      } catch (error) {
        if (controller.signal.aborted) return;
        setArchiveError(archiveFailure(error));
        setReading(false);
      }
    })();
    return () => controller.abort();
  }, [source, label]);

  const level = levels.length > 0 ? levels[levels.length - 1] : null;
  const files = useMemo(() => (level ? level.entries.filter((entry) => !entry.directory) : []), [level]);
  const matched = useMemo(() => {
    const needle = filter.trim().toLowerCase();
    return needle ? files.filter((entry) => entry.name.toLowerCase().includes(needle)) : files;
  }, [files, filter]);
  const shown = matched.slice(0, MAX_ROWS);

  function openLevel(target: number) {
    requestRef.current += 1;
    setLevels((current) => current.slice(0, target + 1));
    setSelection(null);
    setFilter("");
  }

  function openEntry(entry: ZipEntry) {
    if (entry.directory || !level) return;
    const signal = abortRef.current?.signal;
    const token = ++requestRef.current;
    if (!canPreviewFile(entry.name)) {
      setSelection({ entry, name: baseName(entry.name), kind: "unsupported" });
      return;
    }
    if (entry.uncompressedSize > MAX_ENTRY_BYTES) {
      setSelection({ entry, name: baseName(entry.name), kind: "large" });
      return;
    }
    setSelection({ entry, name: baseName(entry.name), kind: "loading" });
    void (async () => {
      try {
        const opened = await entryContents(level.source, entry, signal);
        if (token !== requestRef.current) return;
        if (isArchiveFile(opened.name)) {
          const inner = new MemoryByteSource(opened.bytes);
          const directory = await readZipDirectory(inner, signal);
          if (token !== requestRef.current) return;
          setLevels((current) => [...current, { label: opened.name, source: inner, entries: directory.entries }]);
          setSelection(null);
          setFilter("");
          return;
        }
        // No MIME type: the viewer maps the entry's extension, exactly as it does for an upload.
        setSelection({ entry, name: opened.name, kind: "ready", blob: new Blob([opened.bytes]) });
      } catch (error) {
        if (token !== requestRef.current) return;
        setSelection({ entry, name: baseName(entry.name), kind: "failed", detail: entryFailure(error) });
      }
    })();
  }

  async function downloadEntry(entry: ZipEntry) {
    if (!level) return;
    try {
      const bytes = await readZipEntry(level.source, entry, abortRef.current?.signal);
      const url = URL.createObjectURL(new Blob([bytes]));
      const anchor = document.createElement("a");
      anchor.href = url;
      anchor.download = baseName(entry.name);
      anchor.click();
      setTimeout(() => URL.revokeObjectURL(url), 60_000);
    } catch (error) {
      setSelection({ entry, name: baseName(entry.name), kind: "failed", detail: entryFailure(error) });
    }
  }

  if (reading || archiveError) {
    return (
      <div className={styles.archive}>
        <div className={styles.notice} role={archiveError ? "alert" : "status"}>
          {archiveError ?? t("zip.reading")}
        </div>
      </div>
    );
  }

  return (
    <div className={styles.archive}>
      <header className={styles.header}>
        {levels.length > 1 ? (
          <button className={styles.back} type="button" onClick={() => openLevel(levels.length - 2)}>
            ‹ {t("zip.back")}
          </button>
        ) : null}
        <nav className={styles.crumbs} aria-label={t("zip.contents")}>
          {levels.map((item, index) => (
            <span key={`${item.label}-${index}`} className={styles.crumb}>
              {index > 0 ? <span className={styles.crumbSep}>/</span> : null}
              {index === levels.length - 1 ? (
                <span className={styles.crumbCurrent} title={item.label}>{item.label}</span>
              ) : (
                <button className={styles.crumbButton} type="button" onClick={() => openLevel(index)}>{item.label}</button>
              )}
            </span>
          ))}
        </nav>
        <span className={styles.count}>{filesText(files.length)}</span>
        <input
          className={styles.filter}
          type="search"
          value={filter}
          placeholder={t("zip.filter")}
          aria-label={t("zip.filter")}
          onChange={(event) => setFilter(event.target.value)}
        />
      </header>

      <div className={styles.body}>
        <div className={styles.list} role="listbox" aria-label={t("zip.contents")}>
          {shown.length === 0 ? (
            <p className={styles.listEmpty}>{files.length === 0 ? t("zip.empty") : t("zip.noMatch")}</p>
          ) : (
            shown.map((entry) => (
              <button
                key={entry.name}
                className={selection?.entry === entry ? `${styles.row} ${styles.rowActive}` : styles.row}
                type="button"
                role="option"
                aria-selected={selection?.entry === entry}
                title={entry.name}
                onClick={() => openEntry(entry)}
              >
                <span className={styles.rowName}>{entry.name}</span>
                {opensArchive(entry.name) ? <span className={styles.rowHint}>›</span> : null}
                <span className={styles.rowSize}>{formatSize(entry.uncompressedSize)}</span>
              </button>
            ))
          )}
        </div>

        <div className={styles.pane}>
          {selection ? (
            <>
              <div className={styles.paneHeader}>
                {/* The row's own name and size: the pane downloads this entry, so it must say
                    which entry that is — an inflated entry's inner name belongs to the viewer. */}
                <span className={styles.paneName} title={selection.entry.name}>{selection.entry.name}</span>
                <span className={styles.paneSize}>{formatSize(selection.entry.uncompressedSize)}</span>
                <button
                  className={styles.paneDownload}
                  type="button"
                  title={t("zip.downloadEntry")}
                  aria-label={t("zip.downloadEntry")}
                  onClick={() => void downloadEntry(selection.entry)}
                >
                  <DownloadIcon size={14} />
                </button>
              </div>
              <div className={styles.paneBody}>
                {selection.kind === "loading" ? <p className={styles.hint} role="status">{t("zip.extracting")}</p> : null}
                {selection.kind === "ready" && selection.blob ? (
                  <FileViewer
                    file={selection.blob}
                    fileName={selection.name}
                    width="100%"
                    height="100%"
                    fit="contain"
                    toolbar={false}
                    theme="auto"
                    locale={locale === "zh" ? "zh-CN" : "en-US"}
                    plugins={previewPlugins}
                  />
                ) : null}
                {selection.kind === "failed" ? (
                  <div className={styles.hint}>
                    <strong>{t("zip.entryFailed")}</strong>
                    <span>{selection.detail}</span>
                  </div>
                ) : null}
                {selection.kind === "unsupported" ? (
                  <div className={styles.hint}>
                    <strong>{t("zip.entryUnsupported")}</strong>
                    <span>{t("file.unsupportedText")}</span>
                  </div>
                ) : null}
                {selection.kind === "large" ? (
                  <div className={styles.hint}>
                    <strong>{t("zip.entryTooLarge")}</strong>
                    <span>{t("zip.limit", { size: formatSize(MAX_ENTRY_BYTES) })}</span>
                  </div>
                ) : null}
              </div>
            </>
          ) : (
            <p className={styles.hint}>{t("zip.select")}</p>
          )}
        </div>
      </div>

      {matched.length > shown.length ? (
        <p className={styles.more}>{t("zip.truncated", { shown: shown.length, total: matched.length })}</p>
      ) : null}
    </div>
  );
}

function baseName(name: string): string {
  return name.split("/").pop() || name;
}

/**
 * One entry's bytes, inflated when the entry is itself compressed, plus the name to render them
 * under.
 *
 * `session.v4.jsonl.zstd` is stored in an archive exactly as it is on the drive: a Zstandard
 * frame. The viewer would be handed that frame as if it were text, so the layer is removed here —
 * in memory, with the same bounded chain the standalone preview uses — and `session.v4.jsonl` is
 * what decides the renderer.
 */
async function entryContents(source: ByteSource, entry: ZipEntry, signal?: AbortSignal): Promise<{ name: string; bytes: Bytes }> {
  const bytes = await readZipEntry(source, entry, signal);
  const name = baseName(entry.name);
  if (!compressionKind(name)) return { name, bytes };
  const inner = await inflateChain(bytes, name, { signal });
  return { name: baseName(inner.name), bytes: inner.bytes };
}

/** A row that opens another archive: a ZIP, or a compressed one (`photos.zip.gz`). */
function opensArchive(name: string): boolean {
  return isArchiveFile(baseName(name)) || isArchiveFile(baseName(uncompressedName(name)));
}

function archiveFailure(error: unknown): string {
  if (error instanceof ZipError) {
    switch (error.kind) {
      case "not_archive":
        return t("zip.notArchive");
      case "encrypted":
        return t("zip.encrypted");
      case "too_large":
        return t("zip.tooLarge");
      case "unsupported":
        return t("zip.unsupported");
    }
  }
  return t("zip.readFailed");
}

function entryFailure(error: unknown): string {
  // An entry that is itself compressed can fail on either layer; the inner one has its own
  // sentences, so a damaged Zstandard frame does not read as a damaged archive.
  if (error instanceof CompressionError) {
    switch (error.kind) {
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
    }
  }
  if (error instanceof ZipError) {
    switch (error.kind) {
      case "encrypted":
        return t("zip.entryEncrypted");
      case "too_large":
        return t("zip.entryTooLarge");
      case "unsupported":
        return t("zip.entryUnsupported");
    }
  }
  return t("zip.entryFailed");
}
