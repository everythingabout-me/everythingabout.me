import { useEffect, useState } from "react";
import { useRevalidator } from "react-router";
import { api, type BackupRun, type StorageConnection, type StorageData, type StorageTestResult } from "../api/client";
import { formatBytes, formatSpeed } from "../pages/sizeFormat";
import { t } from "../i18n";
import styles from "./StoragePanels.module.css";

/**
 * Drives are where bytes live, not a backup feature: the service supplies a default
 * drive, an account may attach its own store. An attached store is a backup
 * destination — the account API cannot make one the drive its files live on — so the
 * panel says that instead of implying files could be moved there.
 */
export function DrivesPanel({ data }: { data: StorageData }) {
  const revalidator = useRevalidator();
  const [open, setOpen] = useState(false);
  const [settingsFor, setSettingsFor] = useState("");
  const [testing, setTesting] = useState("");
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

  async function test(connection: StorageConnection) {
    setError("");
    setTesting(connection.id);
    try {
      await api.testStorageConnection(connection.id);
      revalidator.revalidate();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setTesting("");
    }
  }

  return (
    <section className={styles.section}>
      <h2 className={styles.title}>{t("drives.title")}</h2>
      <p className={styles.lead}>{t("drives.lead")}</p>
      <ul className={styles.list}>
        {data.connections.map((connection) => {
          const chosen = connection.id === data.backupTarget;
          const badge =
            connection.role === "authority"
              ? t("drives.roleYourFiles")
              : chosen
                ? t("drives.roleBackupTarget")
                : t("drives.roleBackupOnly");
          return (
            <li className={styles.row} key={connection.id}>
              <div className={styles.rowBody}>
                <div className={styles.name}>
                  {connection.name}
                  <span className={styles.badge}>{badge}</span>
                </div>
                <div className={styles.muted}>{connection.location}</div>
                {connection.backupFolder ? (
                  <div className={styles.muted}>{t("drives.folderShown", { folder: connection.backupFolder })}</div>
                ) : null}
                {connection.probe ? <ProbeLine probe={connection.probe} /> : null}
              </div>
              <div className={styles.actions}>
                {connection.backend !== "local" ? (
                  <button className={styles.quiet} disabled={testing !== ""} onClick={() => void test(connection)}>
                    {testing === connection.id ? t("drives.testing") : t("drives.test")}
                  </button>
                ) : null}
                {connection.mine && connection.role === "backup" ? (
                  <button
                    className={styles.quiet}
                    onClick={() => setSettingsFor(settingsFor === connection.id ? "" : connection.id)}
                  >
                    {t("drives.settings")}
                  </button>
                ) : null}
                {connection.mine ? (
                  <button className={styles.quiet} onClick={() => void remove(connection)}>
                    {t("common.remove")}
                  </button>
                ) : null}
              </div>
              {settingsFor === connection.id ? (
                <ConnectionSettings
                  connection={connection}
                  onCancel={() => setSettingsFor("")}
                  onSaved={() => {
                    setSettingsFor("");
                    revalidator.revalidate();
                  }}
                />
              ) : null}
            </li>
          );
        })}
      </ul>
      <p className={styles.hint}>{t("drives.testHint")}</p>
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

/**
 * A measurement is only useful with all three answers: whether the store answered,
 * how far away it is, and how fast bytes move. A store that answers but cannot be
 * admitted is reported separately, because "connected" is not "ready for backups".
 */
function ProbeLine({ probe }: { probe: StorageTestResult }) {
  if (!probe.ok) {
    return <p className={styles.error}>{t("drives.testFailed", { error: probe.error ?? "" })}</p>;
  }
  if (!probe.backupReady) {
    return <p className={styles.error}>{t("drives.testReachable", { error: probe.error ?? "" })}</p>;
  }
  return (
    <p className={styles.status}>
      {t("drives.testReady", {
        latency: probe.latencyMs,
        up: formatSpeed(probe.uploadBytesPerSecond),
        down: formatSpeed(probe.downloadBytesPerSecond),
      })}
      <span className={styles.mutedInline}>
        {" "}
        · {t("drives.testedAt", { when: testedAgo(probe.testedAt) })}
      </span>
    </p>
  );
}

function testedAgo(iso: string): string {
  const then = new Date(iso).getTime();
  if (!Number.isFinite(then)) return t("drives.testedJustNow");
  const minutes = Math.max(0, Math.round((Date.now() - then) / 60_000));
  if (minutes < 1) return t("drives.testedJustNow");
  if (minutes < 60) return t("drives.testedMinutes", { minutes });
  const hours = Math.round(minutes / 60);
  if (hours < 24) return t("drives.testedHours", { hours });
  return t("drives.testedDays", { days: Math.round(hours / 24) });
}

/**
 * The folder is where dated snapshots land inside the store. It is not the store's
 * location: changing it cannot move the connection, which is why the identity stays
 * the same. Once a date has been written the folder is locked, because moving it
 * would leave listed restore points pointing at a directory nothing writes to.
 */
function ConnectionSettings({
  connection,
  onSaved,
  onCancel,
}: {
  connection: StorageConnection;
  onSaved: () => void;
  onCancel: () => void;
}) {
  const [folder, setFolder] = useState(connection.backupFolder);
  const [error, setError] = useState("");
  const locked = connection.runs > 0;

  return (
    <form
      className={styles.form}
      onSubmit={async (event) => {
        event.preventDefault();
        setError("");
        try {
          await api.updateStorageConnection(connection.id, { backupFolder: folder });
          onSaved();
        } catch (e) {
          setError(e instanceof Error ? e.message : String(e));
        }
      }}
    >
      <label className={styles.field}>
        {t("drives.fieldBackupFolder")}
        <input value={folder} disabled={locked} placeholder="backups" onChange={(event) => setFolder(event.target.value)} />
        <span className={styles.muted}>{t("drives.folderHint")}</span>
      </label>
      {locked ? <p className={styles.hint}>{t("drives.folderLocked", { runs: connection.runs })}</p> : null}
      {error ? <p className={styles.error}>{error}</p> : null}
      <div className={styles.actions}>
        <button className={styles.button} type="submit" disabled={locked}>
          {t("common.save")}
        </button>
        <button className={styles.button} type="button" onClick={onCancel}>
          {t("common.cancel")}
        </button>
      </div>
    </form>
  );
}

function AddDrive({ onAdded, onCancel }: { onAdded: () => void; onCancel: () => void }) {
  const [error, setError] = useState("");
  const [backend, setBackend] = useState<"cos" | "s3">("cos");
  const [pathStyle, setPathStyle] = useState(true);
  const [form, setForm] = useState({ name: "", bucket: "", region: "", endpoint: "", prefix: "backups/", secretId: "", secretKey: "" });
  const field = (key: keyof typeof form) => ({
    value: form[key],
    onChange: (event: React.ChangeEvent<HTMLInputElement>) => setForm({ ...form, [key]: event.target.value }),
  });
  const s3 = backend === "s3";

  return (
    <form
      className={styles.form}
      onSubmit={async (event) => {
        event.preventDefault();
        setError("");
        try {
          await api.addStorageConnection({
            name: form.name,
            backend,
            bucket: form.bucket,
            region: form.region,
            endpoint: s3 ? form.endpoint : undefined,
            pathStyle: s3 ? pathStyle : undefined,
            prefix: s3 ? form.prefix : undefined,
            secretId: form.secretId,
            secretKey: form.secretKey,
          });
          onAdded();
        } catch (e) {
          setError(e instanceof Error ? e.message : String(e));
        }
      }}
    >
      <p className={styles.hint}>{s3 ? t("drives.addHintS3") : t("drives.addHint")}</p>
      <label className={styles.field}>
        {t("drives.fieldBackend")}
        <select value={backend} onChange={(event) => setBackend(event.target.value as "cos" | "s3")}>
          <option value="cos">{t("drives.backendCos")}</option>
          <option value="s3">{t("drives.backendS3")}</option>
        </select>
      </label>
      <label className={styles.field}>
        {t("drives.fieldName")}
        <input required {...field("name")} />
      </label>
      {s3 ? (
        <label className={styles.field}>
          {t("drives.fieldEndpoint")}
          <input required placeholder="https://nas.example.com:9000" {...field("endpoint")} />
        </label>
      ) : null}
      <label className={styles.field}>
        {t("drives.fieldBucket")}
        <input required placeholder={s3 ? "eam-backup" : "example-1250000000"} {...field("bucket")} />
      </label>
      <label className={styles.field}>
        {t("drives.fieldRegion")}
        <input required placeholder={s3 ? "us-east-1" : "ap-hongkong"} {...field("region")} />
      </label>
      {s3 ? (
        <label className={styles.field}>
          {t("drives.fieldPrefix")}
          <input placeholder="backups/" {...field("prefix")} />
          <span className={styles.muted}>{t("drives.fieldPrefixHint")}</span>
        </label>
      ) : null}
      <label className={styles.field}>
        {s3 ? t("drives.fieldAccessKey") : t("drives.fieldSecretId")}
        <input required autoComplete="off" {...field("secretId")} />
      </label>
      <label className={styles.field}>
        {t("drives.fieldSecretKey")}
        <input required type="password" autoComplete="off" {...field("secretKey")} />
      </label>
      {s3 ? (
        <label className={styles.check}>
          <input type="checkbox" checked={pathStyle} onChange={(event) => setPathStyle(event.target.checked)} />
          {t("drives.fieldPathStyle")}
        </label>
      ) : null}
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
  const destinations = data.connections.filter((c) => c.role === "backup");
  const target = destinations.find((c) => c.id === data.backupTarget);
  const running = data.runs.some((run) => run.state === "running");

  // A snapshot runs off the request, so the page has to ask again until it finishes;
  // otherwise a finished backup keeps saying "Running" until the next manual refresh.
  useEffect(() => {
    if (!running) return;
    const timer = setInterval(() => revalidator.revalidate(), 3000);
    return () => clearInterval(timer);
  }, [running, revalidator]);

  async function choose(connectionId: string) {
    setMessage("");
    try {
      await api.setBackupTarget(connectionId);
      revalidator.revalidate();
    } catch (e) {
      setMessage(e instanceof Error ? e.message : String(e));
    }
  }

  async function run() {
    if (!target) return;
    setMessage("");
    try {
      await api.startBackup({ connectionId: target.id, action: "snapshot", keep: data.keep });
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
      {destinations.length === 0 ? (
        <p className={styles.muted}>{t("backup.noTarget")}</p>
      ) : (
        <>
          <label className={styles.field}>
            {t("backup.target")}
            <select value={data.backupTarget} onChange={(event) => void choose(event.target.value)}>
              <option value="">{t("backup.targetNone")}</option>
              {destinations.map((destination) => (
                <option key={destination.id} value={destination.id}>
                  {destination.name}
                </option>
              ))}
            </select>
          </label>
          <div className={styles.actions}>
            <button className={styles.button} disabled={!target || running} onClick={() => void run()}>
              {t("backup.runNow")}
            </button>
          </div>
          {!target ? <p className={styles.muted}>{t("backup.chooseTarget")}</p> : null}
        </>
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
              <tr key={entry.connectionId + entry.snapshot + entry.state + entry.startedAt}>
                <td>
                  {entry.snapshot}
                  <div className={styles.muted}>{entry.connectionName}</div>
                </td>
                <td>{stateLabel(entry)}</td>
                <td>
                  {entry.files > 0
                    ? t("backup.contentsValue", { files: entry.files, size: formatBytes(entry.bytes) })
                    : "—"}
                </td>
                <td>
                  <button
                    className={styles.quiet}
                    disabled={entry.state !== "complete"}
                    onClick={() => void runRestore(entry)}
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

  async function runRestore(entry: BackupRun) {
    setMessage("");
    try {
      // The run names its own destination, so restoring an older date from another
      // store does not require selecting it first.
      await api.startBackup({ connectionId: entry.connectionId, action: "restore", date: entry.snapshot, keep: data.keep });
      setMessage(t("backup.started"));
      revalidator.revalidate();
    } catch (e) {
      setMessage(e instanceof Error ? e.message : String(e));
    }
  }
}

function stateLabel(entry: BackupRun): string {
  if (entry.state === "failed") {
    return entry.error ? t("backup.stateFailedWith", { error: entry.error }) : t("backup.stateFailed");
  }
  return entry.state === "running" ? t("backup.stateRunning") : t("backup.stateComplete");
}
