import { useLoaderData } from "react-router";
import type { AccountSummary } from "../api/client";
import { AccountPanel } from "../components/AccountPanel";
import { AppHeader } from "../components/AppHeader";
import { t } from "../i18n";
import styles from "../styles/auth.module.css";

export function AccountPage() {
  const { summary } = useLoaderData() as { summary: AccountSummary };

  return (
    <div className={styles.page}>
      <title>{t("meta.accountTitle")}</title>
      <AppHeader user={summary.user} />
      <main className={styles.main}>
        <AccountPanel
          user={summary.user}
          legacyCount={summary.legacyCount}
          saveLinks={summary.saveLinks}
          activity={summary.activity}
        />
      </main>
    </div>
  );
}
