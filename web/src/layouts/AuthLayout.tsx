import { Link, Outlet } from "react-router";
import { BrandMark } from "../components/BrandMark";
import { t } from "../i18n";
import styles from "../styles/auth.module.css";

/**
 * The front door: sign-in and choosing an address.
 *
 * Wide screens split the page — the brand and one plain sentence on the left, the form as a card
 * on the right — because a lone 380px card in the middle of a 1440px window leaves the visitor
 * with no idea what they are signing in to. Narrow screens stack the same two blocks in reading
 * order. There is no top bar: this page has no navigation, and the brand already sits in the
 * column beside the form.
 */
export function AuthLayout() {
  return (
    <div className={styles.page}>
      <main className={`${styles.main} ${styles.mainSplit}`}>
        <div className={styles.aside}>
          <Link to="/" className={styles.brand}>
            <BrandMark className={styles.brandMark} />
            <span>{t("app.name")}</span>
          </Link>
          <p className={styles.asideText}>{t("auth.aside")}</p>
        </div>
        <Outlet />
      </main>
      <footer className={styles.legal}>
        <Link to="/privacy">{t("legal.privacy")}</Link>
        <span aria-hidden="true">·</span>
        <Link to="/terms">{t("legal.terms")}</Link>
      </footer>
    </div>
  );
}
