import { Link, useLocation } from "react-router";
import type { AuthUser } from "../api/client";
import { t } from "../i18n";
import { BrandMark } from "./BrandMark";
import styles from "./AppHeader.module.css";

/**
 * The one top bar for every signed-in page: brand, primary navigation (with the current page
 * highlighted) and the account chip on the far right. Page tools such as search and upload stay
 * in the page body, so the header looks the same on every page.
 */
export function AppHeader({ user }: { user: AuthUser }) {
  const { pathname } = useLocation();

  const homeActive = pathname === "/";
  const filesActive = pathname === "/files" || pathname.startsWith("/files/");
  const connectActive = pathname === "/connect";
  const backupActive = pathname === "/backup";

  return (
    <header className={styles.topbar}>
      <div className={styles.topbarInner}>
        <Link className={styles.brand} to="/" title={t("nav.home")}>
          <BrandMark className={styles.brandMark} />
          <span className={styles.brandText}>{t("app.name")}</span>
        </Link>

        <nav className={styles.nav} aria-label={t("nav.primary")}>
          <Link
            className={`${styles.navLink} ${homeActive ? styles.navLinkActive : ""}`}
            to="/"
            aria-current={homeActive ? "page" : undefined}
          >
            {t("nav.stream")}
          </Link>
          <Link
            className={`${styles.navLink} ${filesActive ? styles.navLinkActive : ""}`}
            to="/files"
            aria-current={filesActive ? "page" : undefined}
          >
            {t("nav.files")}
          </Link>
          <Link
            className={`${styles.navLink} ${backupActive ? styles.navLinkActive : ""}`}
            to="/backup"
            aria-current={backupActive ? "page" : undefined}
          >
            {t("nav.backup")}
          </Link>
          <Link
            className={`${styles.navLink} ${connectActive ? styles.navLinkActive : ""}`}
            to="/connect"
            aria-current={connectActive ? "page" : undefined}
          >
            {t("nav.connect")}
          </Link>
        </nav>

        <div className={styles.spacer} />

        <Link
          className={styles.account}
          to="/account"
          title={t("nav.accountSettings")}
          aria-label={t("nav.accountSettingsFor", { email: user.email })}
        >
          <span className={styles.avatar}>{user.email.slice(0, 1).toUpperCase()}</span>
          <span className={styles.email}>{user.email}</span>
        </Link>
      </div>
    </header>
  );
}
