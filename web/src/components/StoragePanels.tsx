import { useEffect, useState } from "react";
import { Link, useRevalidator } from "react-router";
import {
  api,
  type BackupRun,
  type StorageConnection,
  type StorageConnectionDetail,
  type StorageTestResult,
} from "../api/client";
import { formatBytes, formatSpeed } from "../pages/sizeFormat";
import { t } from "../i18n";
import styles from "./StoragePanels.module.css";

/**
 * A provider's card is identity and health only. The credential, the measurement and
 * whatever feature chose it live one level down: a list of four cards that each carry
 * a test button, a folder field and a date table is a page with no index.
 */
export function ProviderCard({ connection, backupTarget }: { connection: StorageConnection; backupTarget: string }) {
  const role = providerRoleLabel(connection, backupTarget);
  return (
    <Link className={styles.card} to={`/settings/storage/${connection.id}`}>
      <span className={styles.cardName}>
        {connection.name}
        <span className={styles.badge}>{role}</span>
      </span>
      <span className={styles.cardLocation}>{connection.location}</span>
      <HealthChip probe={connection.probe} />
      <span className={styles.cardMeta}>{backupLine(connection.lastRun)}</span>
    </Link>
  );
}

/**
 * The badge states the feature's choice, not the provider's history: a provider that
 * still holds older snapshots but is not the current destination would otherwise read
 * as "not used yet" while its own page lists dates.
 */
export function providerRoleLabel(connection: StorageConnection, backupTarget: string): string {
  if (connection.role === "authority") return t("drives.roleYourFiles");
  if (connection.id === backupTarget) return t("drives.roleInUse");
  return connection.lastRun ? t("drives.roleOff") : t("drives.roleUnused");
}

/**
 * Whether the last test still says something: a provider that answered but cannot
 * publish is not "ready", because publication is the promise a backup depends on.
 */
function HealthChip({ probe }: { probe?: StorageTestResult | null }) {
  if (!probe) {
    return (
      <span className={styles.chip}>
        <span className={styles.dot} />
        {t("drives.cardUntested")}
      </span>
    );
  }
  const ready = probe.ok && probe.writable;
  const label = !probe.ok
    ? t("drives.cardUnreachable")
    : probe.writable
      ? t("drives.cardReady")
      : t("drives.cardNotWritable");
  return (
    <span className={styles.chip}>
      <span className={ready ? `${styles.dot} ${styles.dotReady}` : styles.dot} />
      {label}
      <span className={styles.mutedInline}>
        {" · "}
        {testedAgo(probe.testedAt)}
      </span>
    </span>
  );
}

function backupLine(last?: BackupRun | null): string {
  if (!last) return t("drives.cardNoBackup");
  if (last.state === "running") return t("drives.cardBackingUp");
  if (last.state === "failed") return t("drives.cardLastFailed");
  return t("drives.cardLastBackup", { date: last.snapshot });
}

/**
 * A measurement is only useful with all three answers: whether the provider
 * answered, how far away it is, and how fast bytes move. A provider that answers
 * but cannot publish is reported separately, because "connected" is not "usable".
 */
export function ProbeLine({ probe }: { probe: StorageTestResult }) {
  if (!probe.ok) {
    return <p className={styles.error}>{t("drives.testFailed", { error: probe.error ?? "" })}</p>;
  }
  if (!probe.writable) {
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

export function AddProvider({ onAdded, onCancel }: { onAdded: () => void; onCancel: () => void }) {
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
 * Backups are the feature that uses a provider: which provider, and where inside it,
 * are settings of the backup, not of the provider. They are shown here, on the
 * provider that holds the bytes, because a restore point is a directory in *this*
 * store — a page that showed one provider must not list another's dates.
 *
 * "Use this provider" is the one setting that is not local: at most one destination is
 * chosen, so the page names the provider that turning this one on would turn off.
 */
export function BackupSection({ detail }: { detail: StorageConnectionDetail }) {
  const revalidator = useRevalidator();
  const connection = detail.connection;
  const [message, setMessage] = useState("");
  const [saving, setSaving] = useState(false);
  const isTarget = detail.backupTarget?.id === connection.id;
  const replaces = detail.backupTarget && !isTarget ? detail.backupTarget : null;
  const running = detail.runs.some((run) => run.state === "running");

  // A snapshot runs off the request, so the page has to ask again until it finishes;
  // otherwise a finished backup keeps saying "Running" until the next manual refresh.
  useEffect(() => {
    if (!running) return;
    const timer = setInterval(() => revalidator.revalidate(), 3000);
    return () => clearInterval(timer);
  }, [running, revalidator]);

  async function choose(use: boolean) {
    setMessage("");
    setSaving(true);
    try {
      await api.setBackupTarget(use ? connection.id : "");
      revalidator.revalidate();
    } catch (e) {
      setMessage(e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  }

  // A run names its own provider, so running one here does not require that this
  // provider be the chosen destination — the choice only governs the unattended job.
  async function run(action: "snapshot" | "restore", date?: string) {
    setMessage("");
    try {
      await api.startBackup({ connectionId: connection.id, action, date });
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
      <label className={styles.check}>
        <input type="checkbox" checked={isTarget} disabled={saving} onChange={(event) => void choose(event.target.checked)} />
        {t("backup.useThis", { name: connection.name })}
      </label>
      <p className={styles.hint}>
        {isTarget
          ? t("backup.inUse")
          : replaces
            ? t("backup.replaces", { name: replaces.name })
            : t("backup.notChosen")}
      </p>
      <BackupFolderSettings key={connection.id} connection={connection} onSaved={() => revalidator.revalidate()} />
      <div className={styles.actions}>
        <button className={styles.button} disabled={running} onClick={() => void run("snapshot")}>
          {t("backup.runNow")}
        </button>
      </div>
      {message ? <p className={styles.hint}>{message}</p> : null}
      <h3 className={styles.subtitle}>{t("backup.points")}</h3>
      {detail.runs.length === 0 ? (
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
            {detail.runs.map((entry) => (
              <tr key={entry.snapshot + entry.state + entry.startedAt}>
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
                    disabled={entry.state !== "complete" || running}
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

/**
 * The folder is where dated snapshots land inside this provider. It is not the
 * provider's location: changing it cannot move the connection, which is why the
 * identity stays the same. Once a date has been written the folder is locked,
 * because moving it would leave listed restore points pointing at a directory
 * nothing writes to.
 */
function BackupFolderSettings({ connection, onSaved }: { connection: StorageConnection; onSaved: () => void }) {
  const [folder, setFolder] = useState(connection.backupFolder);
  const [error, setError] = useState("");
  const locked = connection.runs > 0;

  return (
    <form
      className={styles.folderForm}
      onSubmit={async (event) => {
        event.preventDefault();
        setError("");
        try {
          await api.setBackupFolder(connection.id, folder);
          onSaved();
        } catch (e) {
          setError(e instanceof Error ? e.message : String(e));
        }
      }}
    >
      <label className={styles.field}>
        {t("backup.folder")}
        <input value={folder} disabled={locked} placeholder="backups" onChange={(event) => setFolder(event.target.value)} />
        <span className={styles.muted}>{t("backup.folderHint", { name: connection.name })}</span>
      </label>
      {locked ? <p className={styles.hint}>{t("backup.folderLocked", { runs: connection.runs })}</p> : null}
      {error ? <p className={styles.error}>{error}</p> : null}
      {locked ? null : (
        <div className={styles.actions}>
          <button className={styles.button} type="submit">
            {t("backup.saveFolder")}
          </button>
        </div>
      )}
    </form>
  );
}

function stateLabel(entry: BackupRun): string {
  if (entry.state === "failed") {
    return entry.error ? t("backup.stateFailedWith", { error: entry.error }) : t("backup.stateFailed");
  }
  return entry.state === "running" ? t("backup.stateRunning") : t("backup.stateComplete");
}
