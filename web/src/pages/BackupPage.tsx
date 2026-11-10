import { useState } from "react";
import { useLoaderData, useRevalidator } from "react-router";
import { api, type AuthUser, type BackupRun, type StorageConnection, type StorageData } from "../api/client";
import { AppHeader } from "../components/AppHeader";
import { formatBytes } from "./sizeFormat";
import { t } from "../i18n";
import styles from "./BackupPage.module.css";

/**
 * Backups are chosen by date, never by file version: the page lists restore points
 * and one destination per row. Providers are deliberately not selectable here --
 * the account can attach its own COS bucket, and that is the only decision it makes.
 */
export function BackupPage() {
  const { user, storage } = useLoaderData() as { user: AuthUser; storage: StorageData };
  const revalidator = useRevalidator();
  const [message, setMessage] = useState("");
  const [busy, setBusy] = useState("");

  const backups = storage.connections.filter((c) => c.role === "backup");

  async function run(action: "snapshot" | "prune" | "restore", connection: StorageConnection, date?: string) {
    setBusy(connection.id + action);
    setMessage("");
    try {
      await api.startBackup({ connectionId: connection.id, action, date, keep: storage.keep });
      setMessage(t("backup.started"));
      revalidator.revalidate();
    } catch (error) {
      setMessage(error instanceof Error ? error.message : String(error));
    } finally {
      setBusy("");
    }
  }

  return (
    <div className={styles.page}>
      <title>{t("meta.backupTitle")}</title>
      <AppHeader user={user} />
      <main className={styles.main}>
        <h1 className={styles.title}>{t("backup.title")}</h1>
        <p className={styles.lead}>{t("backup.lead")}</p>

        {message ? <p className={styles.notice}>{message}</p> : null}

        <section className={styles.section}>
          <h2 className={styles.sectionTitle}>{t("backup.targets")}</h2>
          {backups.length === 0 ? (
            <p className={styles.empty}>{t("backup.noTargets")}</p>
          ) : (
            <ul className={styles.list}>
              {backups.map((connection) => (
                <li className={styles.row} key={connection.id}>
                  <div>
                    <div className={styles.name}>{connection.name}</div>
                    <div className={styles.muted}>{connection.location}</div>
                  </div>
                  <div className={styles.actions}>
                    <button
                      className={styles.button}
                      disabled={busy !== ""}
                      onClick={() => run("snapshot", connection)}
                    >
                      {t("backup.runNow")}
                    </button>
                    <button
                      className={styles.button}
                      disabled={busy !== ""}
                      onClick={() => run("prune", connection)}
                    >
                      {t("backup.prune")}
                    </button>
                    {connection.mine ? (
                      <button
                        className={styles.danger}
                        disabled={busy !== ""}
                        onClick={async () => {
                          await api.removeStorageConnection(connection.id);
                          revalidator.revalidate();
                        }}
                      >
                        {t("common.remove")}
                      </button>
                    ) : null}
                  </div>
                </li>
              ))}
            </ul>
          )}
        </section>

        <AddTarget onAdded={() => revalidator.revalidate()} />

        <section className={styles.section}>
          <h2 className={styles.sectionTitle}>{t("backup.points")}</h2>
          {storage.runs.length === 0 ? (
            <p className={styles.empty}>{t("backup.noPoints")}</p>
          ) : (
            <table className={styles.table}>
              <thead>
                <tr>
                  <th>{t("backup.date")}</th>
                  <th>{t("backup.state")}</th>
                  <th>{t("backup.contents")}</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {storage.runs.map((entry) => (
                  <tr key={entry.snapshot + entry.state}>
                    <td>{entry.snapshot}</td>
                    <td>{stateLabel(entry)}</td>
                    <td>
                      {entry.files > 0
                        ? t("backup.contentsValue", { files: entry.files, size: formatBytes(entry.bytes) })
                        : "—"}
                    </td>
                    <td className={styles.actions}>
                      <button
                        className={styles.button}
                        disabled={busy !== "" || entry.state !== "complete"}
                        onClick={() => {
                          const target = backups[0];
                          if (target) void run("restore", target, entry.snapshot);
                        }}
                      >
                        {t("backup.restore")}
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          <p className={styles.hint}>{t("backup.restoreHint")}</p>
        </section>
      </main>
    </div>
  );
}

function stateLabel(entry: BackupRun): string {
  if (entry.state === "failed") {
    return entry.error ? t("backup.stateFailedWith", { error: entry.error }) : t("backup.stateFailed");
  }
  return entry.state === "running" ? t("backup.stateRunning") : t("backup.stateComplete");
}

function AddTarget({ onAdded }: { onAdded: () => void }) {
  const [open, setOpen] = useState(false);
  const [error, setError] = useState("");
  const [form, setForm] = useState({ name: "", bucket: "", region: "", secretId: "", secretKey: "" });
  const field = (key: keyof typeof form) => ({
    value: form[key],
    onChange: (event: React.ChangeEvent<HTMLInputElement>) => setForm({ ...form, [key]: event.target.value }),
  });

  if (!open) {
    return (
      <section className={styles.section}>
        <button className={styles.button} onClick={() => setOpen(true)}>
          {t("backup.addTarget")}
        </button>
      </section>
    );
  }
  return (
    <section className={styles.section}>
      <h2 className={styles.sectionTitle}>{t("backup.addTargetTitle")}</h2>
      <p className={styles.hint}>{t("backup.addTargetHint")}</p>
      <form
        className={styles.form}
        onSubmit={async (event) => {
          event.preventDefault();
          setError("");
          try {
            await api.addStorageConnection(form);
            setForm({ name: "", bucket: "", region: "", secretId: "", secretKey: "" });
            setOpen(false);
            onAdded();
          } catch (e) {
            setError(e instanceof Error ? e.message : String(e));
          }
        }}
      >
        <label className={styles.field}>
          {t("backup.fieldName")}
          <input required {...field("name")} />
        </label>
        <label className={styles.field}>
          {t("backup.fieldBucket")}
          <input required placeholder="example-1250000000" {...field("bucket")} />
        </label>
        <label className={styles.field}>
          {t("backup.fieldRegion")}
          <input required placeholder="ap-hongkong" {...field("region")} />
        </label>
        <label className={styles.field}>
          {t("backup.fieldSecretId")}
          <input required autoComplete="off" {...field("secretId")} />
        </label>
        <label className={styles.field}>
          {t("backup.fieldSecretKey")}
          <input required type="password" autoComplete="off" {...field("secretKey")} />
        </label>
        {error ? <p className={styles.error}>{error}</p> : null}
        <div className={styles.actions}>
          <button className={styles.button} type="submit">
            {t("common.save")}
          </button>
          <button className={styles.button} type="button" onClick={() => setOpen(false)}>
            {t("common.cancel")}
          </button>
        </div>
      </form>
    </section>
  );
}
