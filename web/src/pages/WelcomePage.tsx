import { useState } from "react";
import { useLoaderData, useNavigate } from "react-router";
import { api, type AuthUser } from "../api/client";
import { filesHref, setDriveBase } from "../filesPath";
import { t } from "../i18n";
import styles from "../styles/auth.module.css";

/**
 * The one step an account must finish before it has an address: choosing its handle.
 *
 * This is deliberately not part of sign-in. An account exists without a handle (email is
 * the login identifier), and folding the name into the credential flow would leave the
 * accounts that signed in before handles existed with no path to one. The drive's home is
 * the handle, so there is no tree to render until this is answered once.
 */
export function WelcomePage() {
  const { user } = useLoaderData() as { user: AuthUser };
  const navigate = useNavigate();
  const [value, setValue] = useState(user.username ?? "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function claim(event: React.FormEvent) {
    event.preventDefault();
    if (busy) return;
    setBusy(true);
    setError(null);
    try {
      const result = await api.setUsername(value.trim());
      setDriveBase(result.user.username);
      navigate(filesHref("/"), { replace: true });
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : t("error.unknown"));
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className={styles.card}>
      <title>{`${t("welcome.title")} · ${t("app.name")}`}</title>
      <h1 className={styles.title}>{t("welcome.title")}</h1>
      <p className={styles.subtitle}>{t("welcome.subtitle")}</p>
      <form className={styles.form} onSubmit={claim}>
        <label className={styles.field}>
          <span className={styles.label}>{t("welcome.label")}</span>
          <input
            className={styles.input}
            value={value}
            autoFocus
            autoComplete="username"
            spellCheck={false}
            placeholder={t("welcome.placeholder")}
            onChange={(event) => setValue(event.target.value)}
          />
        </label>
        {/* The address is the whole point, so show the one this input would produce. */}
        <p className={styles.addressHint}>
          {t("welcome.preview", {
            host: window.location.host,
            handle: value.trim() || "…",
          })}
        </p>
        <p className={styles.fieldHint}>{t("welcome.rules")}</p>
        {error && <p className={styles.error} role="alert">{error}</p>}
        <button
          className={styles.submit}
          type="submit"
          disabled={busy || value.trim() === ""}
        >
          {busy ? t("welcome.claiming") : t("welcome.claim")}
        </button>
      </form>
    </section>
  );
}
