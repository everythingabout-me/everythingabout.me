import { useLoaderData } from "react-router";
import type { AccountSummary, StorageData } from "../api/client";
import { AccountPanel } from "../components/AccountPanel";
import { AppHeader } from "../components/AppHeader";
import { BackupsPanel, DrivesPanel } from "../components/StoragePanels";
import { t } from "../i18n";
import styles from "../styles/auth.module.css";

export function AccountPage() {
  const { summary, storage } = useLoaderData() as { summary: AccountSummary; storage: StorageData };

  return (
    <div className={styles.page}>
      <title>{t("meta.accountTitle")}</title>
      <AppHeader user={summary.user} />
      <main className={styles.accountMain}>
        <AccountPanel
          user={summary.user}
          legacyCount={summary.legacyCount}
          saveLinks={summary.saveLinks}
          activity={summary.activity}
        />
        <div className={styles.accountPanels}>
          <DrivesPanel data={storage} />
          <BackupsPanel data={storage} />
        </div>
      </main>
    </div>
  );
}
