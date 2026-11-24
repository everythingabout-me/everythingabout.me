import { useEffect, useState } from "react";
import { Link, useRevalidator } from "react-router";
import {
  api,
  type BackupRun,
  type FileOpsTest,
  type StorageConnection,
  type StorageConnectionDetail,
  type StorageTestResult,
} from "../api/client";
import { formatBytes, formatSpeed } from "../pages/sizeFormat";
import { t } from "../i18n";
import { CloudIcon, BucketIcon, FolderIcon, ServerIcon } from "./icons";
import styles from "./StoragePanels.module.css";

/**
 * A provider's card is identity and health only. The credential, the measurement and
 * whatever feature chose it live one level down: a list of four cards that each carry
 * a test button, a folder field and a date table is a page with no index.
 *
 * The badge is reserved for the one fact a person came to the list to learn — which
 * provider holds the files, which one backups go to. A badge on every card ("not used
 * yet", "backups off") is a stamp that means nothing, so the rest of the role is left to
 * the detail page, where the choice is actually made.
 */
export function ProviderCard({ connection, backupTarget }: { connection: StorageConnection; backupTarget: string }) {
  const badge =
    connection.role === "authority"
      ? t("drives.roleYourFiles")
      : connection.id === backupTarget
        ? t("drives.roleInUse")
        : null;
  return (
    <Link className={styles.card} to={`/settings/storage/${connection.id}`}>
      <span className={styles.cardTop}>
        <ProviderGlyph connection={connection} />
        <span className={styles.cardIdentity}>
          <span className={styles.cardName}>
            <span className={styles.cardNameText}>{connection.name}</span>
            {badge ? <span className={styles.badge}>{badge}</span> : null}
          </span>
          {connection.location ? (
            <span className={styles.cardLocation}>{connection.location}</span>
          ) : (
            <span className={styles.cardKind}>{storageKindLabel(connection)}</span>
          )}
        </span>
      </span>
      <span className={styles.cardFoot}>
        <HealthChip probe={connection.probe} />
        <span className={styles.cardMeta}>{backupLine(connection.lastRun)}</span>
      </span>
    </Link>
  );
}

/**
 * What kind of store a row is, in the account's words. An account-added provider can
 * also be named by its location, which is the host or bucket the person typed. The
 * deployment's own store has no location to show — that is the operator's
 * infrastructure — so this is the only thing left that says what the card is.
 */
export function storageKindLabel(connection: StorageConnection): string {
  if (!connection.mine) return t("drives.backendManaged");
  return backendLabel(connection.backend);
}

export function backendLabel(backend: string): string {
  if (backend === "cos") return t("drives.backendCos");
  if (backend === "s3") return t("drives.backendS3");
  return t("drives.backendLocal");
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
 * The same glyph on the card and on the page it opens, so the two read as one thing.
 * Which glyph is which store is a fact about who runs it, not only about the protocol:
 * the deployment's own store is a cloud, a bucket the account attached stays a bucket,
 * a self-hosted endpoint is a server, and a host path is a folder. Those are four
 * silhouettes, so the kind is legible without reading the line under the name.
 */
export function ProviderGlyph({ connection, large }: { connection: StorageConnection; large?: boolean }) {
  const Icon = !connection.mine
    ? CloudIcon
    : connection.backend === "cos"
      ? BucketIcon
      : connection.backend === "local"
        ? FolderIcon
        : ServerIcon;
  return (
    <span className={large ? `${styles.glyph} ${styles.glyphLarge}` : styles.glyph}>
      <Icon size={large ? 20 : 17} />
    </span>
  );
}

/**
 * Whether the last test still says something: a provider that answered but cannot
 * publish is not "ready", because publication is the promise a backup depends on.
 */
function HealthChip({ probe }: { probe?: StorageTestResult | null }) {
  const status = probeStatus(probe);
  return (
    <span className={styles.chip} data-tone={status.tone}>
      <span className={styles.dot} />
      <span className={styles.chipLabel}>{status.label}</span>
      {probe?.ok && probe.writable ? <span className={styles.chipValue}>{formatMS(probe.latencyMs)}</span> : null}
    </span>
  );
}

type ProbeTone = "ready" | "failed" | "idle";

/**
 * One place decides how a cached measurement reads, so the card, the panel and the
 * button that produced it cannot disagree about what "writable" means.
 */
function probeStatus(probe?: StorageTestResult | null): { tone: ProbeTone; label: string } {
  if (!probe) return { tone: "idle", label: t("drives.cardUntested") };
  if (!probe.ok) return { tone: "failed", label: t("drives.cardUnreachable") };
  if (!probe.writable) return { tone: "failed", label: t("drives.cardNotWritable") };
  return { tone: "ready", label: t("drives.cardReady") };
}

function backupLine(last?: BackupRun | null): string {
  if (!last) return t("drives.cardNoBackup");
  if (last.state === "running") return t("drives.cardBackingUp");
  if (last.state === "failed") return t("drives.cardLastFailed");
  return t("drives.cardLastBackup", { date: last.snapshot });
}

/**
 * The measurement as a spec sheet: one label, one right-aligned number. The numbers sit in
 * the same position across providers and runs, which is what makes them comparable at a
 * glance; the work behind each row is fixed, so no row needs a footnote to say what it
 * measured. A phase that failed keeps its row and replaces the number with its error, so a
 * provider that is fast at one thing and broken at another says both.
 */
export function ProbeSection({
  probe,
  testing,
  error,
  local,
  onTest,
}: {
  probe?: StorageTestResult | null;
  testing: boolean;
  error: string;
  /** A local root is a host path: it has no credential and no round trip to measure. */
  local?: boolean;
  onTest: () => void;
}) {
  const status = probeStatus(probe);
  return (
    <section className={styles.section}>
      <div className={styles.sectionHead}>
        <div className={styles.sectionHeadText}>
          <h2 className={styles.title}>{t("probe.title")}</h2>
        </div>
        {local ? null : (
          <button className={styles.button} disabled={testing} onClick={onTest} aria-busy={testing}>
            {testing ? t("drives.testing") : t("drives.test")}
          </button>
        )}
      </div>

      {local ? <p className={styles.muted}>{t("probe.local")}</p> : null}

      {local ? null : testing && !probe ? <p className={styles.muted}>{t("drives.testing")}</p> : null}

      {local || probe || testing ? null : (
        <div className={styles.emptyProbe}>
          <p className={styles.emptyTitle}>{t("probe.untested")}</p>
        </div>
      )}

      {local ? null : probe ? (
        <>
          <div className={styles.probeStatus}>
            <span className={`${styles.chip} ${styles.chipPlate}`} data-tone={status.tone}>
              <span className={styles.dot} />
              <span className={styles.chipLabel}>{status.label}</span>
            </span>
            <span className={styles.probeWhen}>{t("drives.testedAt", { when: testedAgo(probe.testedAt) })}</span>
          </div>
          {probe.error ? <p className={styles.error}>{probe.error}</p> : null}
          {probe.ok ? <Measurements probe={probe} /> : null}
        </>
      ) : null}

      {!local && error ? <p className={styles.error}>{error}</p> : null}
    </section>
  );
}

/** One printed row: the label on the left, the number flush right, and only an error below. */
interface MetricRow {
  key: string;
  label: string;
  value: string;
  error?: string;
}

function Measurements({ probe }: { probe: StorageTestResult }) {
  const rows: MetricRow[] = [
    { key: "latency", label: t("probe.latency"), value: formatMS(probe.latencyMs) },
    { key: "upload", label: t("probe.upload"), value: formatSpeed(probe.uploadBytesPerSecond) },
    { key: "download", label: t("probe.download"), value: formatSpeed(probe.downloadBytesPerSecond) },
    fileOpsRow("smallFiles", t("probe.smallFiles"), probe.smallFiles),
    fileOpsRow("renames", t("probe.renames"), probe.renames),
    fileOpsRow("deletes", t("probe.deletes"), probe.deletes),
    {
      key: "concurrency",
      label: t("probe.concurrency"),
      value: probe.concurrency && !probe.concurrency.error ? formatSpeed(probe.concurrency.bytesPerSecond) : "—",
      error: probe.concurrency?.error,
    },
  ];
  return (
    <div className={styles.metrics}>
      {rows.map((row) => (
        <div key={row.key} className={styles.metric}>
          <span className={styles.metricLabel}>{row.label}</span>
          <span className={row.error ? `${styles.metricValue} ${styles.metricValueFailed}` : styles.metricValue}>
            {row.value}
          </span>
          {row.error ? <span className={styles.metricError}>{row.error}</span> : null}
        </div>
      ))}
    </div>
  );
}

function fileOpsRow(key: string, label: string, phase?: FileOpsTest | null): MetricRow {
  if (!phase) return { key, label, value: "—" };
  if (phase.error) return { key, label, value: "—", error: phase.error };
  return { key, label, value: t("probe.perSecond", { value: formatCount(phase.perSecond) }) };
}

/**
 * A per-file-op rate keeps one decimal only below 10: a slow store reports "2.5 files/s"
 * where a fast one reports "33", and a thousandth of a file per second is noise.
 */
function formatCount(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return "0";
  return value >= 10 ? String(Math.round(value)) : value.toFixed(1);
}

/** Sub-millisecond results exist on a local or a very close store; "0 ms" would read as a failure. */
function formatMS(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return "—";
  if (ms < 1) return "< 1 ms";
  return `${Math.round(ms)} ms`;
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
      <div className={styles.sectionHead}>
        <div className={styles.sectionHeadText}>
          <h2 className={styles.title}>{t("backup.title")}</h2>
          <p className={styles.lead}>{t("backup.lead")}</p>
        </div>
        <button className={styles.button} disabled={running} onClick={() => void run("snapshot")}>
          {t("backup.runNow")}
        </button>
      </div>
      <label className={`${styles.check} ${styles.choice}`}>
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
      {message ? <p className={styles.hint}>{message}</p> : null}
      <h3 className={styles.subtitle}>{t("backup.points")}</h3>
      {detail.runs.length === 0 ? (
        <p className={styles.muted}>{t("backup.noPoints")}</p>
      ) : (
        <div className={styles.tableWrap}>
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
        </div>
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
