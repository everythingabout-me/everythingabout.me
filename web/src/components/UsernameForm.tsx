import { useState } from "react";
import { api, type AuthUser } from "../api/client";
import { t } from "../i18n";
import styles from "../styles/auth.module.css";

/**
 * Claiming or renaming the drive's address.
 *
 * A rename is not a credential change: the old handle keeps redirecting, so nothing that
 * already points at this drive stops working. That is why this form needs no verification
 * code, unlike the email form beside it — and why it can say so in one hint.
 */
export function UsernameForm({
  current,
  onSaved,
}: {
  current: string | null;
  onSaved: (user: AuthUser) => void;
}) {
  const [value, setValue] = useState(current ?? "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const handle = value.trim();

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    if (busy) return;
    setBusy(true);
    setError(null);
    setSaved(false);
    try {
      const result = await api.setUsername(handle);
      onSaved(result.user);
      setValue(result.user.username ?? "");
      setSaved(true);
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : t("error.unknown"));
    } finally {
      setBusy(false);
    }
  }

  return (
    <section>
      <h2 className={styles.sectionTitle}>{t("account.addressTitle")}</h2>
      <p className={styles.sectionHint}>{t("account.addressLead")}</p>

      <form className={styles.form} onSubmit={submit}>
        <div className={styles.field}>
          <label className={styles.label} htmlFor="username">
            {t("welcome.label")}
          </label>
          <input
            id="username"
            className={styles.input}
            value={value}
            autoComplete="username"
            spellCheck={false}
            placeholder={t("welcome.placeholder")}
            onChange={(event) => {
              setValue(event.target.value);
              setError(null);
              setSaved(false);
            }}
          />
          <p className={styles.fieldHint}>
            {t("welcome.preview", {
              host: window.location.host,
              handle: handle || "…",
            })}
          </p>
          <p className={styles.fieldHint}>{t("account.addressRules")}</p>
          <p className={styles.fieldHint}>{t("account.addressHint")}</p>
        </div>
        {error && <p className={styles.error} role="alert">{error}</p>}
        {saved && <p className={styles.success}>{t("account.addressSaved")}</p>}
        <button
          className={styles.submit}
          type="submit"
          disabled={busy || handle === "" || handle === current}
        >
          {busy ? t("account.addressSaving") : t("account.addressSave")}
        </button>
      </form>
    </section>
  );
}
