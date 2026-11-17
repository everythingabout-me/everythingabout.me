import { Link } from "react-router";
import type { AuthUser } from "../api/client";
import { t } from "../i18n";
import { BrandMark } from "./BrandMark";
import styles from "./AppHeader.module.css";

/**
 * The one top bar for every signed-in page: the brand is the way home, the account chip is the
 * way into settings. There is no navigation list here on purpose — the home page shows the
 * content and everything that is not content lives in one settings page, so a second row of
 * links would only repeat two entry points the brand and the chip already provide.
 *
 * Page tools such as search and upload stay in the page body.
 */
export function AppHeader({ user }: { user: AuthUser }) {
  return (
    <header className={styles.topbar}>
      <div className={styles.topbarInner}>
        <Link className={styles.brand} to="/" title={t("nav.home")}>
          <BrandMark className={styles.brandMark} />
          <span className={styles.brandText}>{t("app.name")}</span>
        </Link>

        <div className={styles.spacer} />

        <Link
          className={styles.account}
          to="/settings"
          title={t("nav.settings")}
          aria-label={t("nav.settingsFor", { email: user.email })}
        >
          <span className={styles.avatar}>{user.email.slice(0, 1).toUpperCase()}</span>
          <span className={styles.email}>{user.email}</span>
        </Link>
      </div>
    </header>
  );
}
