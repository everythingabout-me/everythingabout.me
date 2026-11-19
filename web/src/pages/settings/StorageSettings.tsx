import { useState } from "react";
import { useLoaderData, useOutletContext, useRevalidator } from "react-router";
import type { StorageData } from "../../api/client";
import { AddProvider, ProviderCard } from "../../components/StoragePanels";
import { PlusIcon } from "../../components/icons";
import { legacyFilesText, t } from "../../i18n";
import type { SettingsContext } from "../SettingsPage";
import panel from "../../components/StoragePanels.module.css";
import styles from "./settings.module.css";

/**
 * Storage is a list of places bytes can live, one card each. What a feature does with a
 * place — a backup destination, its folder, the dates it holds — is shown on that
 * provider's own page, because those dates are directories in that store and nowhere
 * else.
 */
export function StorageSettings() {
  const { data } = useLoaderData() as { data: StorageData };
  const { summary } = useOutletContext<SettingsContext>();
  const revalidator = useRevalidator();
  const [adding, setAdding] = useState(false);

  return (
    <div className={styles.stack}>
      <div className={panel.grid}>
        {data.connections.map((connection) => (
          <ProviderCard key={connection.id} connection={connection} backupTarget={data.backupTarget} />
        ))}
        {adding ? null : (
          <button className={panel.addTile} onClick={() => setAdding(true)}>
            <span className={panel.addTileIcon}>
              <PlusIcon size={15} />
            </span>
            {t("drives.add")}
          </button>
        )}
      </div>

      {adding ? (
        <section className={panel.section}>
          <h2 className={panel.title}>{t("drives.add")}</h2>
          <AddProvider
            onCancel={() => setAdding(false)}
            onAdded={() => {
              setAdding(false);
              revalidator.revalidate();
            }}
          />
        </section>
      ) : null}

      {/* Only an account that still has files from before it existed sees this. */}
      {summary.legacyCount > 0 && (
        <section className={panel.section}>
          <p className={panel.hint}>
            {t("account.legacyHint", { files: legacyFilesText(summary.legacyCount) })}
            {t("account.legacyRun")}
            <code>{`eam claim --email ${summary.user.email}`}</code>
            {t("account.legacyClaim")}
          </p>
        </section>
      )}
    </div>
  );
}
