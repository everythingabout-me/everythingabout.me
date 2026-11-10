import { useState } from "react";
import { useRevalidator } from "react-router";
import { api, type BackupRun, type StorageConnection, type StorageData } from "../api/client";
import { formatBytes } from "../pages/sizeFormat";
import { t } from "../i18n";
import styles from "./StoragePanels.module.css";

/**
 * Drives are where bytes live, not a backup feature: the service supplies a default
 * drive, an account may attach its own. Backups are a separate concern that happens
 * to use a drive, so they get their own panel below.
 */
export function DrivesPanel({ data }: { data: StorageData }) {
  const revalidator = useRevalidator();
  const [open, setOpen] = useState(false);
  const [error, setError] = useState("");

  async function remove(connection: StorageConnection) {
    setError("");
    try {
      await api.removeStorageConnection(connection.id);
      revalidator.revalidate();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }

  return (
    <section className={styles.section}>
      <h2 className={styles.title}>{t("drives.title")}</h2>
      <p className={styles.lead}>{t("drives.lead")}</p>
      <ul className={styles.list}>
        {data.connections.map((connection) => (
          <li className={styles.row} key={connection.id}>
            <div>
              <div className={styles.name}>
                {connection.name}
                <span className={styles.badge}>
                  {connection.mine
                    ? t("drives.mine")
                    : connection.role === "authority"
                      ? t("drives.default")
                      : t("drives.roleBackup")}
                </span>
              </div>
              <div className={styles.muted}>{connection.location}</div>
            </div>
            {connection.mine ? (
              <button className={styles.quiet} onClick={() => void remove(connection)}>
                {t("common.remove")}
              </button>
            ) : null}
          </li>
        ))}
      </ul>
      {error ? <p className={styles.error}>{error}</p> : null}
      {open ? (
        <AddDrive
          onCancel={() => setOpen(false)}
          onAdded={() => {
            setOpen(false);
            revalidator.revalidate();
          }}
        />
      ) : (
        <button className={styles.button} onClick={() => setOpen(true)}>
          {t("drives.add")}
        </button>
      )}
    </section>
  );
}

function AddDrive({ onAdded, onCancel }: { onAdded: () => void; onCancel: () => void }) {
  const [error, setError] = useState("");
  const [form, setForm] = useState({ name: "", bucket: "", region: "", secretId: "", secretKey: "" });
  const field = (key: keyof typeof form) => ({
    value: form[key],
    onChange: (event: React.ChangeEvent<HTMLInputElement>) => setForm({ ...form, [key]: event.target.value }),
  });

  return (
    <form
      className={styles.form}
      onSubmit={async (event) => {
        event.preventDefault();
        setError("");
        try {
          await api.addStorageConnection(form);
          onAdded();
        } catch (e) {
          setError(e instanceof Error ? e.message : String(e));
        }
      }}
    >
      <p className={styles.hint}>{t("drives.addHint")}</p>
      <label className={styles.field}>
        {t("drives.fieldName")}
        <input required {...field("name")} />
      </label>
      <label className={styles.field}>
        {t("drives.fieldBucket")}
        <input required placeholder="example-1250000000" {...field("bucket")} />
      </label>
      <label className={styles.field}>
        {t("drives.fieldRegion")}
        <input required placeholder="ap-hongkong" {...field("region")} />
      </label>
      <label className={styles.field}>
        {t("drives.fieldSecretId")}
        <input required autoComplete="off" {...field("secretId")} />
      </label>
      <label className={styles.field}>
        {t("drives.fieldSecretKey")}
        <input required type="password" autoComplete="off" {...field("secretKey")} />
      </label>
      {error ? <p className={styles.error}>{error}</p> : null}
      <div className={styles.actions}>
        <button className={styles.button} type="submit">
          {t("common.save")}
        </button>
        <button className={styles.button} type="button" onClick={onCancel}>
          {t("common.cancel")}
        </button>
      </div>
    </form>
  );
}

/**
 * Restore points, one row per date. A restore copies the date to a new folder on the
 * owner's tree and refuses to overwrite anything, so a wrong choice is recoverable.
 */
export function BackupsPanel({ data }: { data: StorageData }) {
  const revalidator = useRevalidator();
  const [message, setMessage] = useState("");
  const target = data.connections.find((c) => c.role === "backup");

  async function run(action: "snapshot" | "restore", date?: string) {
    if (!target) return;
    setMessage("");
    try {
      await api.startBackup({ connectionId: target.id, action, date, keep: data.keep });
      setMessage(t("backup.started"));
      revalidator.revalidate();
    } catch (e) {
      setMessage(e instanceof Error ? e.message : String(e));
    }
  }

  return (
    <section className={styles.section}>
      <h2 className={styles.title}>{t("backup.title")}</h2>
      <p className={styles.lead}>{t("backup.lead")}</p>
      {target ? (
        <div className={styles.actions}>
          <button className={styles.button} onClick={() => void run("snapshot")}>
            {t("backup.runNow")}
          </button>
          <span className={styles.muted}>{target.name}</span>
        </div>
      ) : (
        <p className={styles.muted}>{t("backup.noTarget")}</p>
      )}
      {message ? <p className={styles.hint}>{message}</p> : null}
      {data.runs.length === 0 ? (
        <p className={styles.muted}>{t("backup.noPoints")}</p>
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
            {data.runs.map((entry) => (
              <tr key={entry.snapshot + entry.state}>
                <td>{entry.snapshot}</td>
                <td>{stateLabel(entry)}</td>
                <td>
                  {entry.files > 0
                    ? t("backup.contentsValue", { files: entry.files, size: formatBytes(entry.bytes) })
                    : "—"}
                </td>
                <td>
                  <button
                    className={styles.quiet}
                    disabled={entry.state !== "complete" || !target}
                    onClick={() => void run("restore", entry.snapshot)}
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
  );
}

function stateLabel(entry: BackupRun): string {
  if (entry.state === "failed") {
    return entry.error ? t("backup.stateFailedWith", { error: entry.error }) : t("backup.stateFailed");
  }
  return entry.state === "running" ? t("backup.stateRunning") : t("backup.stateComplete");
}
