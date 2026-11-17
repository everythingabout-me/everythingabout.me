import { useState } from "react";
import { Link, useLoaderData, useNavigate, useRevalidator } from "react-router";
import { api, type StorageConnectionDetail } from "../../api/client";
import { BackupSection, ProbeLine, providerRoleLabel } from "../../components/StoragePanels";
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
  const [testing, setTesting] = useState(false);
  // Two blocks, two errors: a failed measurement must not appear under the remove
  // button, where it would read as the reason removal failed.
  const [testError, setTestError] = useState("");
  const [removeError, setRemoveError] = useState("");
  const roleLabel = providerRoleLabel(connection, data.backupTarget?.id ?? "");

  async function test() {
    setTestError("");
    setTesting(true);
    try {
      await api.testStorageConnection(connection.id);
      revalidator.revalidate();
    } catch (e) {
      setTestError(e instanceof Error ? e.message : String(e));
    } finally {
      setTesting(false);
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
      <div className={panel.head}>
        <Link className={panel.backLink} to="/settings/storage">
          {t("provider.back")}
        </Link>
        <h3 className={panel.providerName}>
          {connection.name}
          <span className={panel.badge}>{roleLabel}</span>
        </h3>
        <p className={panel.providerMeta}>
          {backendLabel(connection.backend)} · {connection.location}
        </p>
        {authority ? <p className={panel.muted}>{t("provider.authorityNote")}</p> : null}
      </div>

      {/* A local root is a host path: it has no credential and no round trip to measure. */}
      {connection.backend !== "local" ? (
        <section className={panel.section}>
          <h2 className={panel.title}>{t("provider.reachability")}</h2>
          {connection.probe ? (
            <ProbeLine probe={connection.probe} />
          ) : (
            <p className={panel.muted}>{t("provider.neverTested")}</p>
          )}
          <p className={panel.hint}>{t("drives.testHint")}</p>
          <div className={panel.actions}>
            <button className={panel.button} disabled={testing} onClick={() => void test()}>
              {testing ? t("drives.testing") : t("drives.test")}
            </button>
          </div>
          {testError ? <p className={panel.error}>{testError}</p> : null}
        </section>
      ) : null}

      {authority ? (
        <section className={panel.section}>
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

function backendLabel(backend: string): string {
  if (backend === "cos") return t("drives.backendCos");
  if (backend === "s3") return t("drives.backendS3");
  return t("drives.backendLocal");
}
