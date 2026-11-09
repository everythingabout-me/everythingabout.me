import { Link } from "react-router";
import { t } from "../i18n";
import styles from "./StorageNotice.module.css";

/**
 * The one thing a person can do when the index and storage disagree: let the account page
 * compare them and apply the difference. The drive is the source of truth, so this is a
 * pointer to that flow rather than a second sync implementation.
 */
export function StorageNotice({ missingContent = false }: { missingContent?: boolean }) {
  return (
    <div className={styles.notice} role="status">
      <div className={styles.copy}>
        <span className={styles.title}>
          {t(missingContent ? "storageDrift.contentTitle" : "storageDrift.pathTitle")}
        </span>
        <span className={styles.text}>
          {t(missingContent ? "storageDrift.contentText" : "storageDrift.pathText")}
        </span>
      </div>
      <Link className={styles.action} to="/account#storage">
        {t("storageDrift.action")}
      </Link>
    </div>
  );
}
