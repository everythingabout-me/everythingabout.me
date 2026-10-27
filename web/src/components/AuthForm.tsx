import { useEffect, useState } from "react";
import { useNavigate } from "react-router";
import { api } from "../api/client";
import { t } from "../i18n";
import styles from "../styles/auth.module.css";

type Mode = "code" | "password";

export function AuthForm({ initialMode = "code" }: { initialMode?: Mode }) {
  const navigate = useNavigate();
  const mode = initialMode;
  const [email, setEmail] = useState("");
  const [code, setCode] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [sendingCode, setSendingCode] = useState(false);
  const [cooldown, setCooldown] = useState(0);
  const [codeNotice, setCodeNotice] = useState<string | null>(null);

  useEffect(() => {
    if (cooldown <= 0) return;
    const timer = window.setTimeout(() => setCooldown((value) => value - 1), 1000);
    return () => window.clearTimeout(timer);
  }, [cooldown]);

  async function sendCode() {
    if (!email.trim()) {
      setError(t("auth.emailRequired"));
      return;
    }

    setError(null);
    setCodeNotice(null);
    setSendingCode(true);
    try {
      const result = await api.sendCode(email);
      const minutes = Math.round((result.expiresIn ?? 600) / 60);
      if (result.devCode) {
        setCode(result.devCode);
        setCodeNotice(t("auth.devCodeNotice", { code: result.devCode }));
      } else {
        setCode("");
        setCodeNotice(t("auth.codeSentNotice", { minutes }));
      }
      setCooldown(60);
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : t("error.sendCodeFailed"));
    } finally {
      setSendingCode(false);
    }
  }

  async function onSubmit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError(null);

    if (mode === "code" && !/^\d{6}$/.test(code.trim())) {
      setError(t("auth.codeRequired"));
      return;
    }
    if (mode === "password" && !password) {
      setError(t("auth.passwordRequired"));
      return;
    }

    setBusy(true);
    try {
      if (mode === "code") {
        await api.verifyCode(email, code);
      } else {
        await api.loginWithPassword(email, password);
      }
      // 登录态是 httpOnly cookie；跳回 / 时由 loader 重新确认
      navigate("/", { replace: true });
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : t("error.operationFailed"));
      setBusy(false);
    }
  }

  return (
    <form className={styles.form} onSubmit={onSubmit}>
      <div className={styles.field}>
        <label className={styles.label} htmlFor="signin-email">
          {t("auth.email")}
        </label>
        <div className={styles.inputRow}>
          <input
            id="signin-email"
            className={styles.input}
            type="email"
            name="email"
            autoComplete="email"
            inputMode="email"
            placeholder="you@example.com"
            value={email}
            onChange={(event) => {
              setEmail(event.target.value);
              if (mode === "code") {
                setCode("");
                setCooldown(0);
                setCodeNotice(null);
                setError(null);
              }
            }}
            autoFocus
            required
          />
          {mode === "code" && (
            <button
              type="button"
              className={styles.codeBtn}
              onClick={() => void sendCode()}
              disabled={sendingCode || cooldown > 0 || !email.trim()}
            >
              {sendingCode
                ? t("auth.sending")
                : cooldown > 0
                  ? `${cooldown}s`
                  : t("auth.sendCode")}
            </button>
          )}
        </div>
      </div>

      {mode === "code" ? (
        <div className={styles.field}>
          <label className={styles.label} htmlFor="signin-code">
            {t("auth.code")}
          </label>
          <input
            id="signin-code"
            className={styles.input}
            type="text"
            name="code"
            inputMode="numeric"
            autoComplete="one-time-code"
            pattern="[0-9]{6}"
            maxLength={6}
            placeholder={t("auth.codePlaceholder")}
            value={code}
            onChange={(event) => setCode(event.target.value.replace(/\D/g, "").slice(0, 6))}
            required
          />
          {codeNotice && <p className={styles.fieldHint}>{codeNotice}</p>}
        </div>
      ) : (
        <div className={styles.field}>
          <label className={styles.label} htmlFor="signin-password">
            {t("auth.password")}
          </label>
          <input
            id="signin-password"
            className={styles.input}
            type="password"
            name="password"
            autoComplete="current-password"
            placeholder={t("auth.passwordPlaceholder")}
            value={password}
            onChange={(event) => setPassword(event.target.value)}
            required
          />
        </div>
      )}

      {error && (
        <p className={styles.error} role="alert">
          {error}
        </p>
      )}

      <button className={styles.submit} type="submit" disabled={busy}>
        {busy ? t("auth.pleaseWait") : mode === "code" ? t("auth.continue") : t("auth.signIn")}
      </button>
    </form>
  );
}
