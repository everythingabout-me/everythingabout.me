import { useState } from "react";
import { useNavigate } from "react-router";
import { api, type AuthUser } from "../api/client";
import { legacyFilesText, t } from "../i18n";
import { ChangeEmailForm } from "./ChangeEmailForm";
import { PasswordForm } from "./PasswordForm";
import styles from "../styles/auth.module.css";

export function AccountPanel({ user, legacyCount }: { user: AuthUser; legacyCount: number }) {
  const navigate = useNavigate();
  const [email, setEmail] = useState(user.email);
  const [hasPassword, setHasPassword] = useState(user.hasPassword);
  const [busy, setBusy] = useState(false);

  const createdAt = user.createdAt.slice(0, 10);

  async function logout() {
    setBusy(true);
    try {
      await api.logout();
    } finally {
      navigate("/login", { replace: true });
    }
  }

  return (
    <section className={`${styles.card} ${styles.cardWide}`}>
      <div className={styles.cardHead}>
        <div>
          <h1 className={styles.title}>{t("account.title")}</h1>
          <p className={styles.subtitle}>{t("account.subtitle")}</p>
        </div>
        <button className={styles.ghostBtn} onClick={() => void logout()} disabled={busy}>
          {t("account.signOut")}
        </button>
      </div>

      <dl className={styles.metaList}>
        <div className={styles.metaRow}>
          <dt className={styles.metaLabel}>{t("account.emailLabel")}</dt>
          <dd className={styles.metaValue}>{email}</dd>
        </div>
        <div className={styles.metaRow}>
          <dt className={styles.metaLabel}>{t("account.joinedLabel")}</dt>
          <dd className={styles.metaValue}>{createdAt}</dd>
        </div>
      </dl>

      <div className={styles.divider} />
      <ChangeEmailForm currentEmail={email} onChanged={setEmail} />

      <div className={styles.divider} />
      <PasswordForm hasPassword={hasPassword} onChanged={setHasPassword} />

      {legacyCount > 0 && (
        <>
          <div className={styles.divider} />
          <p className={styles.footnote}>
            {t("account.legacyHint", { files: legacyFilesText(legacyCount) })}
            {t("account.legacyRun")}
            <code className={styles.command}>
              eam claim --email {email}
            </code>
            {t("account.legacyClaim")}
          </p>
        </>
      )}
    </section>
  );
}
