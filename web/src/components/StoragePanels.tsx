import { useEffect, useRef, useState } from "react";
import { Link, useRevalidator } from "react-router";
import {
  api,
  ApiError,
  type BackupRun,
  type BrowserProbeGrant,
  type StorageConnection,
  type StorageConnectionDetail,
  type StorageProduct,
} from "../api/client";
import { formatBytes, formatSpeed } from "../pages/sizeFormat";
import { t } from "../i18n";
import { BucketIcon, FolderIcon } from "./icons";
import { BrandMark } from "./BrandMark";
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
  // The product is the thing a person chose and the thing the guarantees come from, so it
  // is the label; "S3-compatible" described a protocol family that answers none of the
  // questions a connection raises.
  switch (connection.product) {
    case "aws-s3":
      return t("drives.productAwsS3");
    case "cos":
      return t("drives.productCos");
    case "minio":
      return t("drives.productMinio");
    case "rustfs":
      return t("drives.productRustfs");
    case "local":
      return t("drives.backendLocal");
    case "managed":
      return t("drives.backendManaged");
    case "s3":
      // A row written before the product split names only the protocol family. It has no
      // product to print and cannot publish until an operator names one, so the label says
      // what the row actually says instead of inventing a product.
      return t("drives.backendS3");
  }
  return connection.product;
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
 * The store's kind, which is the one thing the card's mark carries. It is decided once, here:
 * the glyph, the plate's colour and the label all read it, so a card cannot show a cloud in
 * amber and call itself S3.
 */
export type StorageKind = "managed" | "cos" | "s3" | "local";

export function storageKind(connection: StorageConnection): StorageKind {
  if (!connection.mine) return "managed";
  if (connection.backend === "cos") return "cos";
  if (connection.backend === "local") return "local";
  return "s3";
}

/**
 * The same mark on the card and on the page it opens, so the two read as one thing.
 *
 * Three of the four are products rather than shapes, and are drawn as the products: the
 * store this service runs is the service itself, so it wears the circle mark instead of a
 * generic cloud; an S3-compatible endpoint wears "S3" rather than a server rack that says
 * nothing about which protocol answers on it; an attached bucket and a host path stay a
 * bucket and a folder, the two marks a person already reads that way. The plate takes the
 * kind's hue so the kinds separate before the line under the name is read.
 */
export function ProviderGlyph({ connection, large }: { connection: StorageConnection; large?: boolean }) {
  const kind = storageKind(connection);
  return (
    <span className={large ? `${styles.glyph} ${styles.glyphLarge}` : styles.glyph} data-kind={kind}>
      {kind === "managed" ? (
        <BrandMark size={large ? 28 : 22} />
      ) : kind === "s3" ? (
        <span className={styles.glyphWord}>S3</span>
      ) : kind === "cos" ? (
        <BucketIcon size={large ? 18 : 15} />
      ) : (
        <FolderIcon size={large ? 18 : 15} />
      )}
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
 * The speed test. There is one, and this is it: it runs in the browser, over the path an
 * upload takes on this connection, so upload and download are the numbers a person actually
 * feels when they move a file.
 *
 * The server decides the leg, not the page. A product that a browser can be signed into gets
 * a conditional PUT and a read on its own URLs; everything else gets this service's own two
 * endpoints, which relay the bytes through the same provider calls a real upload makes. The
 * two legs differ in one visible way — a direct leg carries no cookie — and that difference
 * arrives in the grant rather than being guessed here.
 *
 * Nothing here is kept. The run names one disposable object and the object is deleted in the
 * finally, because a client's speed is a fact about a network at a moment, not a property of
 * the provider worth caching.
 */
export function SpeedTest({ connectionId }: { connectionId: string }) {
  const [running, setRunning] = useState(false);
  const [elapsed, setElapsed] = useState(0);
  const startedAt = useRef(0);
  // A phase rather than a percentage: each half is one request, so the honest progress is
  // which half is running and how long it has been. A 100 MB upload on a home uplink takes
  // long enough that a bare spinner would not say what is happening.
  const [phase, setPhase] = useState<"upload" | "download" | null>(null);
  const [bytes, setBytes] = useState(0);
  /** What the store actually accepted, which is what "sent" may claim. */
  const [sent, setSent] = useState(0);
  const [latencyMS, setLatencyMS] = useState<number | null>(null);
  const [uploadBps, setUploadBps] = useState(0);
  const [downloadBps, setDownloadBps] = useState(0);
  /** Whether the bytes are going to the provider itself, which changes what a CORS failure means. */
  const [direct, setDirect] = useState(false);
  // A store this service cannot reach as an object store (a local root) has no speed to
  // measure. The 409's message is a full sentence written for the person, so it reads as the
  // explanation it is instead of as a red line under the button.
  const [explanation, setExplanation] = useState("");
  const [error, setError] = useState("");

  useEffect(() => {
    if (!running) return;
    const timer = setInterval(() => setElapsed(Math.round((Date.now() - startedAt.current) / 1000)), 500);
    return () => clearInterval(timer);
  }, [running]);

  /**
   * The store answered without the object. The status is the whole message: the body is the
   * provider's XML about its own internals, which is neither actionable here nor ours to
   * print.
   */
  function refusal(response: Response): string {
    return t("probe.refused", { status: response.status });
  }

  async function measure(grant: BrowserProbeGrant) {
    // One allocation for the whole body. Writing it in chunks would measure this loop, and
    // allocating per chunk would measure the garbage collector; a 100 MB buffer that throws
    // says so in the error line, which is the only useful thing left to report.
    const body = new Uint8Array(grant.bytes);

    // Every signed header but Content-Length: fetch derives it from the body and refuses a
    // caller-set value. The rest — the content type and the "only if absent" condition — are
    // the grant's terms, so dropping one would change what is being measured.
    const uploadHeaders: Record<string, string> = {};
    for (const [name, value] of Object.entries(grant.upload.headers ?? {})) {
      if (name.toLowerCase() === "content-length") continue;
      uploadHeaders[name] = value;
    }

    setPhase("upload");
    const uploadStart = performance.now();
    const uploaded = await fetch(grant.upload.url, {
      method: grant.upload.method ?? "PUT",
      headers: uploadHeaders,
      body,
      // Set by the grant: a provider URL must not carry this service's cookie, and a relayed
      // leg is this service and needs it.
      credentials: grant.upload.credentials,
      cache: "no-store",
    });
    const uploadMS = performance.now() - uploadStart;
    if (!uploaded.ok) throw new Error(refusal(uploaded));
    setSent(grant.bytes);
    setUploadBps(rate(grant.bytes, uploadMS));

    setPhase("download");
    const downloadStart = performance.now();
    const downloaded = await fetch(grant.download.url, {
      headers: grant.download.headers ?? {},
      credentials: grant.download.credentials,
      cache: "no-store",
    });
    // The response resolves once the first byte and the headers are here: that wait is the
    // round trip a person feels before a file starts moving.
    const firstByteMS = performance.now() - downloadStart;
    if (!downloaded.ok) throw new Error(refusal(downloaded));
    setLatencyMS(firstByteMS);
    const reader = downloaded.body!.getReader();
    const bodyStart = performance.now();
    let received = 0;
    for (;;) {
      const chunk = await reader.read();
      if (chunk.done) break;
      received += chunk.value.byteLength;
    }
    setDownloadBps(rate(received, performance.now() - bodyStart));
  }

  async function run() {
    startedAt.current = Date.now();
    setElapsed(0);
    setRunning(true);
    setExplanation("");
    setError("");
    setPhase(null);
    setBytes(0);
    setSent(0);
    setLatencyMS(null);
    setUploadBps(0);
    setDownloadBps(0);
    try {
      let grant: BrowserProbeGrant;
      try {
        grant = await api.startBrowserProbe(connectionId);
      } catch (e) {
        if (e instanceof ApiError && e.status === 409) {
          setExplanation(e.message);
          return;
        }
        throw e;
      }
      setBytes(grant.bytes);
      setDirect(grant.direct);
      try {
        await measure(grant);
      } finally {
        // The object is disposable and the run is over either way. Cleanup failing says
        // nothing about the numbers already taken, so it must not replace them.
        try {
          await api.deleteBrowserProbe(connectionId, grant.runId);
        } catch {
          /* a leaked probe object is a few minutes of one key, not a failed measurement */
        }
      }
    } catch (e) {
      // A request that never gets a status is a network failure. On a direct leg the usual
      // cause is a missing CORS rule on the store, because the provider is being asked to
      // accept a cross-origin write; on a relayed leg there is no cross-origin request to
      // fail, so it is the connection itself.
      if (e instanceof TypeError) {
        setError(`${e.message} ${direct ? t("probe.networkDirect") : t("probe.networkRelay")}`);
      } else {
        setError(e instanceof Error ? e.message : String(e));
      }
    } finally {
      setPhase(null);
      setRunning(false);
    }
  }

  const rows = [
    {
      key: "latency",
      label: t("probe.latency"),
      value: latencyMS === null ? "" : formatMS(latencyMS),
      busy: phase === "download" && latencyMS === null,
    },
    {
      key: "upload",
      label: t("probe.upload"),
      value: uploadBps > 0 ? formatSpeed(uploadBps) : "",
      busy: phase === "upload",
    },
    {
      key: "download",
      label: t("probe.download"),
      value: downloadBps > 0 ? formatSpeed(downloadBps) : "",
      busy: phase === "download",
    },
  ];

  return (
    <section className={styles.section}>
      <div className={styles.sectionHead}>
        <div className={styles.sectionHeadText}>
          <h2 className={styles.title}>{t("probe.title")}</h2>
          <p className={styles.lead}>{t("probe.lead")}</p>
        </div>
        <button className={styles.button} disabled={running} aria-busy={running} onClick={() => void run()}>
          {running ? t("probe.running", { seconds: elapsed }) : t("probe.run")}
        </button>
      </div>

      {explanation ? <p className={styles.lead}>{explanation}</p> : null}
      {error ? (
        <p className={styles.error}>
          {t("probe.failed")} {error}
        </p>
      ) : null}

      {bytes > 0 ? (
        <div className={styles.metrics} aria-busy={running ? true : undefined}>
          {rows.map((row) => (
            <div key={row.key} className={styles.metric}>
              <span className={styles.metricLabel}>{row.label}</span>
              {row.busy ? (
                <span className={`${styles.metricValue} ${styles.metricBusy}`}>
                  <span className={styles.spinner} aria-hidden="true" />
                </span>
              ) : (
                <span className={styles.metricValue}>{row.value}</span>
              )}
            </div>
          ))}
        </div>
      ) : null}

      {running && phase ? (
        <p className={styles.muted}>
          {phase === "upload" ? t("probe.uploading", { size: formatBytes(bytes) }) : t("probe.downloading")}
        </p>
      ) : null}

      {sent > 0 ? <p className={styles.hint}>{t("probe.sent", { size: formatBytes(sent) })}</p> : null}
      <p className={styles.muted}>{t("probe.notSaved")}</p>
    </section>
  );
}

/** Sub-millisecond results exist on a very close store; "0 ms" would read as a failure. */
function formatMS(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return "—";
  if (ms < 1) return "< 1 ms";
  return `${Math.round(ms)} ms`;
}

/** Bytes per second over one transfer, tolerant of a clock that did not advance. */
function rate(bytes: number, milliseconds: number): number {
  return milliseconds > 0 ? bytes / (milliseconds / 1000) : 0;
}

export function AddProvider({ onAdded, onCancel }: { onAdded: () => void; onCancel: () => void }) {
  const [error, setError] = useState("");
  const [product, setProduct] = useState<StorageProduct>("cos");
  const [pathStyle, setPathStyle] = useState(true);
  const [form, setForm] = useState({ name: "", bucket: "", region: "", endpoint: "", prefix: "backups/", secretId: "", secretKey: "" });
  const field = (key: keyof typeof form) => ({
    value: form[key],
    onChange: (event: React.ChangeEvent<HTMLInputElement>) => setForm({ ...form, [key]: event.target.value }),
  });
  // COS signs virtual-hosted URLs; the two self-hosted products are normally addressed by
  // path. Picking the product is what selects the driver and the write guarantees, so it
  // is the first field and the one that decides what the rest of the form asks for.
  const s3 = product !== "cos";
  const chooseProduct = (next: StorageProduct) => {
    setProduct(next);
    setPathStyle(next !== "aws-s3");
  };

  return (
    <form
      className={styles.form}
      onSubmit={async (event) => {
        event.preventDefault();
        setError("");
        try {
          await api.addStorageConnection({
            name: form.name,
            product,
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
      <label className={styles.field}>
        {t("drives.fieldProduct")}
        <select value={product} onChange={(event) => chooseProduct(event.target.value as StorageProduct)}>
          <option value="cos">{t("drives.productCos")}</option>
          <option value="aws-s3">{t("drives.productAwsS3")}</option>
          <option value="minio">{t("drives.productMinio")}</option>
          <option value="rustfs">{t("drives.productRustfs")}</option>
        </select>
        <span className={styles.muted}>{t("drives.productHint")}</span>
      </label>
      <p className={styles.hint}>{s3 ? t("drives.addHintS3") : t("drives.addHint")}</p>
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
