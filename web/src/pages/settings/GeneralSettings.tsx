import { useState } from "react";
import { useNavigate, useOutletContext } from "react-router";
import { api } from "../../api/client";
import { ChangeEmailForm } from "../../components/ChangeEmailForm";
import { UsernameForm } from "../../components/UsernameForm";
import { PasswordForm } from "../../components/PasswordForm";
import { t } from "../../i18n";
import authStyles from "../../styles/auth.module.css";
import type { SettingsContext } from "../SettingsPage";
import styles from "./settings.module.css";

/**
 * General is the account itself: what identifies it (email, signup date), the two credentials it
 * has (the email address and an optional password), and the one action that ends this session.
 * Everything that reaches into the drive lives under Connect and Storage instead.
 */
export function GeneralSettings() {
  const { summary } = useOutletContext<SettingsContext>();
  const navigate = useNavigate();
  const [email, setEmail] = useState(summary.user.email);
  const [username, setUsername] = useState(summary.user.username);
  const [hasPassword, setHasPassword] = useState(summary.user.hasPassword);
  const [busy, setBusy] = useState(false);

  async function signOut() {
    setBusy(true);
    try {
      await api.logout();
    } finally {
      navigate("/login", { replace: true });
    }
  }

  // `authStyles.cardWide` is the marker the account forms look for to render a compact submit
  // button; it has no width of its own inside the settings pane.
  const roomy = `${styles.card} ${authStyles.cardWide}`;

  return (
    <div className={styles.stack}>
      <section className={styles.card}>
        <h2 className={styles.cardTitle}>{t("account.title")}</h2>
        <p className={styles.cardLead}>{t("account.subtitle")}</p>
        <dl className={authStyles.metaList}>
          <div className={authStyles.metaRow}>
            <dt className={authStyles.metaLabel}>{t("account.emailLabel")}</dt>
            <dd className={authStyles.metaValue}>{email}</dd>
          </div>
          <div className={authStyles.metaRow}>
            <dt className={authStyles.metaLabel}>{t("account.joinedLabel")}</dt>
            <dd className={authStyles.metaValue}>{summary.user.createdAt.slice(0, 10)}</dd>
          </div>
        </dl>
      </section>

      <section className={roomy}>
        <UsernameForm
          current={username}
          onSaved={(user) => setUsername(user.username)}
        />
      </section>

      <section className={roomy}>
        <ChangeEmailForm currentEmail={email} onChanged={setEmail} />
      </section>

      <section className={roomy}>
        <PasswordForm hasPassword={hasPassword} onChanged={setHasPassword} />
      </section>

      <section className={styles.card}>
        <div className={styles.cardHead}>
          <div className={styles.cardHeadText}>
            <h2 className={styles.cardTitle}>{t("account.signOut")}</h2>
            <p className={styles.cardLead}>{t("settings.general.signOutHint")}</p>
          </div>
          <button
            className={`${authStyles.ghostBtn} ${styles.cardAction}`}
            type="button"
            disabled={busy}
            onClick={() => void signOut()}
          >
            {t("account.signOut")}
          </button>
        </div>
      </section>
    </div>
  );
}
