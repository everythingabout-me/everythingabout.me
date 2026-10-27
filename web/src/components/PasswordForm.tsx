import { useState } from "react";
import { api } from "../api/client";
import { t } from "../i18n";
import styles from "../styles/auth.module.css";

export function PasswordForm({
  hasPassword,
  onChanged,
}: {
  hasPassword: boolean;
  onChanged: (hasPassword: boolean) => void;
}) {
  const [currentPassword, setCurrentPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  async function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError(null);
    setNotice(null);

    if (newPassword !== confirm) {
      setError(t("account.password.mismatch"));
      return;
    }

    setBusy(true);
    try {
      const result = await api.setPassword({
        password: newPassword,
        ...(hasPassword ? { currentPassword } : {}),
      });
      onChanged(result.user.hasPassword);
      setCurrentPassword("");
      setNewPassword("");
      setConfirm("");
      setNotice(
        hasPassword ? t("account.password.updated") : t("account.password.setDone"),
      );
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : t("error.saveFailed"));
    } finally {
      setBusy(false);
    }
  }

  async function removePassword() {
    setError(null);
    setNotice(null);
    if (!currentPassword) {
      setError(t("account.password.currentRequired"));
      return;
    }

    setBusy(true);
    try {
      const result = await api.removePassword(currentPassword);
      onChanged(result.user.hasPassword);
      setCurrentPassword("");
      setNewPassword("");
      setConfirm("");
      setNotice(t("account.password.removed"));
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : t("error.removeFailed"));
    } finally {
      setBusy(false);
    }
  }

  return (
    <section>
      <h2 className={styles.sectionTitle}>
        {hasPassword ? t("account.password.title") : t("account.password.titleOptional")}
      </h2>
      <p className={styles.sectionHint}>
        {hasPassword
          ? t("account.password.hint")
          : t("account.password.hintOptional")}
      </p>

      <form className={styles.form} onSubmit={submit}>
        {hasPassword && (
          <div className={styles.field}>
            <label className={styles.label} htmlFor="current-password">
              {t("account.password.current")}
            </label>
            <input
              id="current-password"
              className={styles.input}
              type="password"
              name="currentPassword"
              autoComplete="current-password"
              placeholder={t("account.password.currentPlaceholder")}
              value={currentPassword}
              onChange={(event) => setCurrentPassword(event.target.value)}
              required
            />
          </div>
        )}

        <div className={styles.field}>
          <label className={styles.label} htmlFor="new-password">
            {t("account.password.new")}
          </label>
          <input
            id="new-password"
            className={styles.input}
            type="password"
            name="password"
            autoComplete="new-password"
            placeholder={t("account.password.newPlaceholder")}
            value={newPassword}
            onChange={(event) => setNewPassword(event.target.value)}
            minLength={8}
            required
          />
        </div>

        <div className={styles.field}>
          <label className={styles.label} htmlFor="confirm-password">
            {t("account.password.confirm")}
          </label>
          <input
            id="confirm-password"
            className={styles.input}
            type="password"
            name="confirm"
            autoComplete="new-password"
            placeholder={t("account.password.confirmPlaceholder")}
            value={confirm}
            onChange={(event) => setConfirm(event.target.value)}
            minLength={8}
            required
          />
        </div>

        {error && (
          <p className={styles.error} role="alert">
            {error}
          </p>
        )}
        {notice && <p className={styles.success}>{notice}</p>}

        <button className={styles.submit} type="submit" disabled={busy}>
          {busy
            ? t("account.password.save")
            : hasPassword
              ? t("account.password.update")
              : t("account.password.set")}
        </button>

        {hasPassword && (
          <button
            type="button"
            className={styles.removeBtn}
            onClick={() => void removePassword()}
            disabled={busy}
          >
            {t("account.password.remove")}
          </button>
        )}
      </form>
    </section>
  );
}
