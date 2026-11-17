import { useLoaderData, useOutletContext } from "react-router";
import type { StorageData } from "../../api/client";
import { BackupsPanel, DrivesPanel } from "../../components/StoragePanels";
import { StorageSync } from "../../components/StorageSync";
import { legacyFilesText, t } from "../../i18n";
import type { SettingsContext } from "../SettingsPage";
import styles from "./settings.module.css";

/**
 * Storage is the byte layer: the providers that can hold files, the backup feature that copies
 * the tree to one of them, and the sync that makes the index follow bytes moved outside the app.
 */
export function StorageSettings() {
  const { data } = useLoaderData() as { data: StorageData };
  const { summary } = useOutletContext<SettingsContext>();

  return (
    <div className={styles.stack}>
      <DrivesPanel data={data} />
      <BackupsPanel data={data} />
      <section className={styles.card}>
        <StorageSync />
      </section>

      {/* Only an account that still has files from before it existed sees this. */}
      {summary.legacyCount > 0 && (
        <section className={styles.card}>
          <p className={styles.hint}>
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
