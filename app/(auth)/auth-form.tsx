"use client";

import { useEffect, useState } from "react";
import styles from "./auth.module.css";

type Mode = "code" | "password";

export function AuthForm({
  initialMode = "code",
}: {
  initialMode?: Mode;
}) {
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
      setError("请先填写邮箱");
      return;
    }

    setError(null);
    setCodeNotice(null);
    setSendingCode(true);
    try {
      const response = await fetch("/api/auth/code", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ email }),
      });
      const payload = (await response.json()) as {
        ok: boolean;
        error?: string;
        data?: { expiresIn?: number; devCode?: string };
      };
      if (!payload.ok) throw new Error(payload.error ?? "验证码发送失败");

      const minutes = Math.round((payload.data?.expiresIn ?? 600) / 60);
      if (payload.data?.devCode) {
        setCode(payload.data.devCode);
        setCodeNotice(`开发模式：验证码 ${payload.data.devCode} 已自动填入`);
      } else {
        setCode("");
        setCodeNotice(`验证码已发送，请查收邮箱（${minutes} 分钟内有效）`);
      }
      setCooldown(60);
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "验证码发送失败");
    } finally {
      setSendingCode(false);
    }
  }

  async function onSubmit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError(null);

    if (mode === "code" && !/^\d{6}$/.test(code.trim())) {
      setError("请输入 6 位验证码");
      return;
    }
    if (mode === "password" && !password) {
      setError("请输入密码");
      return;
    }

    setBusy(true);
    try {
      const endpoint = mode === "code" ? "/api/auth/verify" : "/api/auth/login";
      const payloadBody =
        mode === "code" ? { email, code } : { email, password };
      const response = await fetch(endpoint, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(payloadBody),
      });
      const payload = (await response.json()) as { ok: boolean; error?: string };
      if (!payload.ok) throw new Error(payload.error ?? "操作失败");
      // 整页跳转，确保服务端组件重新读取登录态
      window.location.assign("/");
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "操作失败");
      setBusy(false);
    }
  }

  return (
    <form className={styles.form} onSubmit={onSubmit}>
      <div className={styles.field}>
        <label className={styles.label} htmlFor="signin-email">
          邮箱
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
              {sendingCode ? "发送中…" : cooldown > 0 ? `${cooldown}s` : "发送验证码"}
            </button>
          )}
        </div>
      </div>

      {mode === "code" ? (
        <div className={styles.field}>
          <label className={styles.label} htmlFor="signin-code">
            验证码
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
            placeholder="6 位数字"
            value={code}
            onChange={(event) =>
              setCode(event.target.value.replace(/\D/g, "").slice(0, 6))
            }
            required
          />
          {codeNotice && <p className={styles.fieldHint}>{codeNotice}</p>}
        </div>
      ) : (
        <div className={styles.field}>
          <label className={styles.label} htmlFor="signin-password">
            密码
          </label>
          <input
            id="signin-password"
            className={styles.input}
            type="password"
            name="password"
            autoComplete="current-password"
            placeholder="输入密码"
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
        {busy ? "请稍候…" : mode === "code" ? "继续" : "登录"}
      </button>
    </form>
  );
}
