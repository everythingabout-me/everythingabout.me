import { useEffect, useState } from "react";
import { api } from "../api/client";
import { t } from "../i18n";
import styles from "../styles/auth.module.css";

export function ChangeEmailForm({
  currentEmail,
  onChanged,
}: {
  currentEmail: string;
  onChanged: (email: string) => void;
}) {
  const [newEmail, setNewEmail] = useState("");
  const [oldCode, setOldCode] = useState("");
  const [newCode, setNewCode] = useState("");
  const [oldSending, setOldSending] = useState(false);
  const [newSending, setNewSending] = useState(false);
  const [oldCooldown, setOldCooldown] = useState(0);
  const [newCooldown, setNewCooldown] = useState(0);
  const [oldNotice, setOldNotice] = useState<string | null>(null);
  const [newNotice, setNewNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState<string | null>(null);

  useEffect(() => {
    if (oldCooldown <= 0) return;
    const timer = window.setTimeout(() => setOldCooldown((value) => value - 1), 1000);
    return () => window.clearTimeout(timer);
  }, [oldCooldown]);

  useEffect(() => {
    if (newCooldown <= 0) return;
    const timer = window.setTimeout(() => setNewCooldown((value) => value - 1), 1000);
    return () => window.clearTimeout(timer);
  }, [newCooldown]);

  function codeNotice(devCode: string | undefined, target: "current" | "new") {
    const where = t(
      target === "current"
        ? "account.changeEmail.currentWhere"
        : "account.changeEmail.newWhere",
    );
    return devCode
      ? t("account.changeEmail.devCode", { where, code: devCode })
      : t("account.changeEmail.sentCode", { where });
  }

  async function sendOldCode() {
    setError(null);
    setSuccess(null);
    setOldSending(true);
    try {
      const result = await api.sendCurrentEmailCode();
      if (result.devCode) setOldCode(result.devCode);
      setOldNotice(codeNotice(result.devCode, "current"));
      setOldCooldown(60);
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : t("error.sendCodeFailed"));
    } finally {
      setOldSending(false);
    }
  }

  async function sendNewCode() {
    setError(null);
    setSuccess(null);
    if (!newEmail.trim()) {
      setError(t("account.changeEmail.newEmailRequired"));
      return;
    }

    setNewSending(true);
    try {
      const result = await api.sendNewEmailCode(newEmail);
      if (result.devCode) setNewCode(result.devCode);
      setNewNotice(codeNotice(result.devCode, "new"));
      setNewCooldown(60);
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : t("error.sendCodeFailed"));
    } finally {
      setNewSending(false);
    }
  }

  async function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError(null);
    setSuccess(null);

    if (!/^\d{6}$/.test(oldCode.trim())) {
      setError(t("account.changeEmail.currentCodeRequired"));
      return;
    }
    if (!/^\d{6}$/.test(newCode.trim())) {
      setError(t("account.changeEmail.newCodeRequired"));
      return;
    }

    setBusy(true);
    try {
      const result = await api.changeEmail({ email: newEmail, oldCode, code: newCode });
      onChanged(result.user.email);
      setNewEmail("");
      setOldCode("");
      setNewCode("");
      setOldNotice(null);
      setNewNotice(null);
      setOldCooldown(0);
      setNewCooldown(0);
      setSuccess(t("account.changeEmail.success"));
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : t("error.updateFailed"));
    } finally {
      setBusy(false);
    }
  }

  return (
    <section>
      <h2 className={styles.sectionTitle}>{t("account.changeEmail.title")}</h2>
      <p className={styles.sectionHint}>{t("account.changeEmail.hint")}</p>

      <form className={styles.form} onSubmit={submit}>
        <div className={styles.field}>
          <label className={styles.label} htmlFor="current-email-display">
            {t("account.changeEmail.currentEmail")}
          </label>
          <div className={styles.inputRow}>
            <input
              id="current-email-display"
              className={`${styles.input} ${styles.readonlyInput}`}
              type="email"
              value={currentEmail}
              readOnly
            />
            <button
              type="button"
              className={styles.codeBtn}
              onClick={() => void sendOldCode()}
              disabled={oldSending || oldCooldown > 0}
            >
              {oldSending
                ? t("auth.sending")
                : oldCooldown > 0
                  ? `${oldCooldown}s`
                  : t("auth.sendCode")}
            </button>
          </div>
        </div>

        <div className={styles.field}>
          <label className={styles.label} htmlFor="old-code">
            {t("account.changeEmail.currentCode")}
          </label>
          <input
            id="old-code"
            className={styles.input}
            type="text"
            name="oldCode"
            inputMode="numeric"
            autoComplete="one-time-code"
            pattern="[0-9]{6}"
            maxLength={6}
            placeholder={t("account.changeEmail.digits")}
            value={oldCode}
            onChange={(event) => {
              setOldCode(event.target.value.replace(/\D/g, "").slice(0, 6));
              setError(null);
              setSuccess(null);
            }}
            required
          />
          {oldNotice && <p className={styles.fieldHint}>{oldNotice}</p>}
        </div>

        <div className={styles.field}>
          <label className={styles.label} htmlFor="new-email">
            {t("account.changeEmail.newEmail")}
          </label>
          <div className={styles.inputRow}>
            <input
              id="new-email"
              className={styles.input}
              type="email"
              name="email"
              autoComplete="email"
              placeholder="new@example.com"
              value={newEmail}
              onChange={(event) => {
                setNewEmail(event.target.value);
                setNewCode("");
                setNewCooldown(0);
                setNewNotice(null);
                setError(null);
                setSuccess(null);
              }}
              required
            />
            <button
              type="button"
              className={styles.codeBtn}
              onClick={() => void sendNewCode()}
              disabled={newSending || newCooldown > 0 || !newEmail.trim()}
            >
              {newSending
                ? t("auth.sending")
                : newCooldown > 0
                  ? `${newCooldown}s`
                  : t("auth.sendCode")}
            </button>
          </div>
        </div>

        <div className={styles.field}>
          <label className={styles.label} htmlFor="new-code">
            {t("account.changeEmail.newCode")}
          </label>
          <input
            id="new-code"
            className={styles.input}
            type="text"
            name="code"
            inputMode="numeric"
            autoComplete="one-time-code"
            pattern="[0-9]{6}"
            maxLength={6}
            placeholder={t("account.changeEmail.digits")}
            value={newCode}
            onChange={(event) => {
              setNewCode(event.target.value.replace(/\D/g, "").slice(0, 6));
              setError(null);
              setSuccess(null);
            }}
            required
          />
          {newNotice && <p className={styles.fieldHint}>{newNotice}</p>}
        </div>

        {error && (
          <p className={styles.error} role="alert">
            {error}
          </p>
        )}
        {success && <p className={styles.success}>{success}</p>}

        <button className={styles.submit} type="submit" disabled={busy}>
          {busy ? t("account.changeEmail.saving") : t("account.changeEmail.save")}
        </button>
      </form>
    </section>
  );
}
