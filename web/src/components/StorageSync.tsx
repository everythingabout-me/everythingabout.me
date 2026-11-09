import { useEffect, useState } from "react";
import { api, type FileAudit, type FileSyncReport } from "../api/client";
import { t } from "../i18n";
import styles from "../styles/auth.module.css";

const MAX_LISTED = 20;

/**
 * Storage is the source of truth and the index is its readable cache, so the only
 * user action is a refresh: preview the differences, then apply them. Removals are
 * recorded in the audit trail and listed here afterwards.
 */
export function StorageSync() {
  const [busy, setBusy] = useState<"preview" | "apply" | null>(null);
  const [report, setReport] = useState<FileSyncReport | null>(null);
  const [failed, setFailed] = useState(false);
  const [records, setRecords] = useState<FileAudit[]>([]);

  useEffect(() => {
    void loadRecords();
    // Another page links here as /account#storage; React Router does not scroll to a
    // hash target that mounts after navigation, so settle it once the panel exists.
    if (window.location.hash === "#storage") {
      document.getElementById("storage")?.scrollIntoView({ block: "start" });
    }
  }, []);

  async function loadRecords() {
    try {
      const page = await api.audit({ limit: 50 });
      setRecords(page.items.filter((item) => item.reasonCode === "native_absent").slice(0, MAX_LISTED));
    } catch {
      // Records are informational; failing to read them must not block syncing.
    }
  }

  async function run(dryRun: boolean) {
    setBusy(dryRun ? "preview" : "apply");
    setFailed(false);
    try {
      const result = await api.syncFiles(dryRun);
      setReport(result);
      if (!dryRun) await loadRecords();
    } catch {
      setFailed(true);
    } finally {
      setBusy(null);
    }
  }

  function apply() {
    const removed = report?.removed ?? 0;
    if (!window.confirm(t("account.storage.confirm", { count: removed }))) return;
    void run(false);
  }

  const unchanged =
    report !== null && report.inserted === 0 && report.updated === 0 && report.removed === 0;
  const removedPaths = report?.removedPaths ?? [];

  return (
    <div className={styles.syncPanel} id="storage">
      <h2 className={styles.syncTitle}>{t("account.storage.title")}</h2>
      <p className={styles.footnote}>{t("account.storage.hint")}</p>
      <div className={styles.syncActions}>
        <button className={styles.ghostBtn} disabled={busy !== null} onClick={() => void run(true)}>
          {busy === "preview" ? t("account.storage.running") : t("account.storage.preview")}
        </button>
        <button className={styles.ghostBtn} disabled={busy !== null || report === null} onClick={apply}>
          {busy === "apply" ? t("account.storage.running") : t("account.storage.apply")}
        </button>
      </div>
      {report !== null && (
        <p className={styles.footnote}>
          {unchanged
            ? t("account.storage.inSync")
            : t("account.storage.result", {
                scanned: report.scanned,
                inserted: report.inserted,
                updated: report.updated,
                removed: report.removed,
              })}
        </p>
      )}
      {removedPaths.length > 0 && (
        <ul className={styles.syncList}>
          {removedPaths.slice(0, MAX_LISTED).map((path) => (
            <li key={path} className={styles.syncItem}>
              <code>{path}</code>
            </li>
          ))}
        </ul>
      )}
      {failed && <p className={styles.footnote}>{t("account.storage.failed")}</p>}

      <h3 className={styles.syncSubtitle}>{t("account.storage.records")}</h3>
      <p className={styles.footnote}>{t("account.storage.recordsHint")}</p>
      {records.length === 0 ? (
        <p className={styles.footnote}>{t("account.storage.recordsEmpty")}</p>
      ) : (
        <ul className={styles.syncList}>
          {records.map((record) => (
            <li key={record.id} className={styles.syncItem}>
              <code>{record.path ?? ""}</code>
              <span>{record.observedAt.slice(0, 10)}</span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
