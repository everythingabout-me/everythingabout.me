import { useState } from "react";
import { Link, useLoaderData, useNavigate } from "react-router";
import { api, type StorageConnectionDetail } from "../../api/client";
import {
  BackupSection,
  ProviderGlyph,
  providerRoleLabel,
  SpeedTest,
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
  const navigate = useNavigate();
  const connection = data.connection;
  const authority = connection.role === "authority";
  // Two blocks, two errors: a failed measurement must not appear under the remove
  // button, where it would read as the reason removal failed.
  const [removeError, setRemoveError] = useState("");
  const roleLabel = providerRoleLabel(connection, data.backupTarget?.id ?? "");

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

      {connection.backend === "local" ? (
        <section className={panel.section}>
          <h2 className={panel.title}>{t("probe.title")}</h2>
          <p className={panel.muted}>{t("probe.local")}</p>
        </section>
      ) : (
        <SpeedTest connectionId={connection.id} />
      )}

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
