import { useEffect, useRef, useState } from "react";
import { Link, useLoaderData, useNavigate, useRevalidator } from "react-router";
import { api, type StorageConnectionDetail } from "../../api/client";
import {
  BackupSection,
  BrowserProbeSection,
  ProbeSection,
  ProviderGlyph,
  providerRoleLabel,
  storageKindLabel,
} from "../../components/StoragePanels";
import { StorageSync } from "../../components/StorageSync";
import { t } from "../../i18n";
import panel from "../../components/StoragePanels.module.css";

/**
 * One provider's page. The two roles get different blocks rather than a disabled copy of
 * each other's: the provider your files live on can never be a backup destination (the
 * service refuses it, because writing a dated copy into the tree being copied is not a
 * backup), and it is the only tree this service keeps an index of — so the refresh
 * action belongs there and nowhere else.
 */
export function StorageProviderSettings() {
  const { data } = useLoaderData() as { data: StorageConnectionDetail };
  const revalidator = useRevalidator();
  const navigate = useNavigate();
  const connection = data.connection;
  const authority = connection.role === "authority";
  // The measurement runs off the request, so the page waits for the run it started and
  // then revalidates until that run's answer is cached on the connection. The poll also
  // carries the run's progress, which is why it is fast enough to watch: a row appears
  // about as soon as its phase finishes, not when the whole test does.
  const [runId, setRunId] = useState<string | null>(null);
  const startedAt = useRef(0);
  const polls = useRef(0);
  // Two blocks, two errors: a failed measurement must not appear under the remove
  // button, where it would read as the reason removal failed.
  const [testError, setTestError] = useState("");
  const [removeError, setRemoveError] = useState("");
  const roleLabel = providerRoleLabel(connection, data.backupTarget?.id ?? "");
  // A page reloaded mid-test knows the server is still working even though it no longer
  // knows the run id, so the panel stays in its measuring state until the server says so.
  const testing = runId !== null || connection.probeRunning === true;

  useEffect(() => {
    if (!testing) return;
    if (runId !== null && connection.probe?.runId === runId) {
      setRunId(null);
      return;
    }
    // One poll has happened by now, so "not measuring and no answer for this run" means the
    // run ended without caching a result (a restart, or a failed write of the result), not
    // that the first request has not reached the server yet.
    if (runId !== null && polls.current > 0 && connection.probeRunning !== true) {
      setRunId(null);
      setTestError(t("probe.stalled"));
      return;
    }
    // A run that outlives every plausible measurement is stuck in this process; say so
    // instead of leaving the page saying "measuring" with no way out.
    if (runId !== null && Date.now() - startedAt.current > 10 * 60_000) {
      setRunId(null);
      setTestError(t("probe.stalled"));
      return;
    }
    const poll = setInterval(() => {
      polls.current += 1;
      revalidator.revalidate();
    }, 1000);
    return () => clearInterval(poll);
  }, [testing, runId, connection.probe?.runId, connection.probeRunning, revalidator]);

  async function test() {
    setTestError("");
    try {
      const started = await api.testStorageConnection(connection.id);
      startedAt.current = Date.now();
      polls.current = 0;
      setRunId(started.runId);
      revalidator.revalidate();
    } catch (e) {
      setTestError(e instanceof Error ? e.message : String(e));
    }
  }

  async function remove() {
    if (!window.confirm(t("provider.removeConfirm", { name: connection.name }))) return;
    setRemoveError("");
    try {
      await api.removeStorageConnection(connection.id);
      void navigate("/settings/storage");
    } catch (e) {
      setRemoveError(e instanceof Error ? e.message : String(e));
    }
  }

  return (
    <div className={panel.pageStack}>
      <div className={panel.backBar}>
        <Link className={panel.backLink} to="/settings/storage">
          ← {t("provider.back")}
        </Link>
      </div>

      <header className={panel.identity}>
        <ProviderGlyph connection={connection} large />
        <div className={panel.identityText}>
          <h3 className={panel.providerName}>
            {connection.name}
            <span className={panel.badge}>{roleLabel}</span>
          </h3>
          <p className={panel.providerMeta}>
            <span>{storageKindLabel(connection)}</span>
            {/* The deployment's own store has no location the account may read. */}
            {connection.location ? (
              <>
                <span className={panel.metaSep}>·</span>
                <code className={panel.locationCode}>{connection.location}</code>
              </>
            ) : null}
          </p>
          {authority ? <p className={panel.identityNote}>{t("provider.authorityNote")}</p> : null}
        </div>
      </header>

      <ProbeSection
        probe={connection.probe}
        progress={connection.probeProgress}
        testing={testing}
        error={testError}
        local={connection.backend === "local"}
        onTest={() => void test()}
      />

      <BrowserProbeSection connectionId={connection.id} />

      {authority ? (
        <section className={panel.section}>
          <h2 className={panel.title}>{t("provider.indexTitle")}</h2>
          <p className={panel.lead}>{t("provider.indexLead")}</p>
          <StorageSync />
        </section>
      ) : (
        <BackupSection detail={data} />
      )}

      {connection.mine ? (
        <section className={panel.section}>
          <h2 className={panel.title}>{t("provider.removeTitle")}</h2>
          <p className={panel.lead}>{t("provider.removeLead")}</p>
          {removeError ? <p className={panel.error}>{removeError}</p> : null}
          <div className={panel.actions}>
            <button className={panel.danger} onClick={() => void remove()}>
              {t("common.remove")}
            </button>
          </div>
        </section>
      ) : null}
    </div>
  );
}
