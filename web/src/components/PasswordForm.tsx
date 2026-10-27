import { useState } from "react";
import { api } from "../api/client";
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
      setError("两次输入的密码不一致");
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
      setNotice(hasPassword ? "密码已更新。" : "密码已设置，现在也可以用密码登录。");
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "save failed");
    } finally {
      setBusy(false);
    }
  }

  async function removePassword() {
    setError(null);
    setNotice(null);
    if (!currentPassword) {
      setError("请先填写当前密码");
      return;
    }

    setBusy(true);
    try {
      const result = await api.removePassword(currentPassword);
      onChanged(result.user.hasPassword);
      setCurrentPassword("");
      setNewPassword("");
      setConfirm("");
      setNotice("密码已移除，之后请用邮箱验证码登录。");
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "remove failed");
    } finally {
      setBusy(false);
    }
  }

  return (
    <section>
      <h2 className={styles.sectionTitle}>{hasPassword ? "登录密码" : "登录密码（可选）"}</h2>
      <p className={styles.sectionHint}>
        {hasPassword
          ? "已设置密码，可以用密码登录；也可以移除，只保留邮箱验证码。"
          : "当前只用邮箱验证码登录。设置密码后，也可以选择用密码登录。"}
      </p>

      <form className={styles.form} onSubmit={submit}>
        {hasPassword && (
          <div className={styles.field}>
            <label className={styles.label} htmlFor="current-password">
              当前密码
            </label>
            <input
              id="current-password"
              className={styles.input}
              type="password"
              name="currentPassword"
              autoComplete="current-password"
              placeholder="确认是你本人"
              value={currentPassword}
              onChange={(event) => setCurrentPassword(event.target.value)}
              required
            />
          </div>
        )}

        <div className={styles.field}>
          <label className={styles.label} htmlFor="new-password">
            新密码
          </label>
          <input
            id="new-password"
            className={styles.input}
            type="password"
            name="password"
            autoComplete="new-password"
            placeholder="至少 8 位"
            value={newPassword}
            onChange={(event) => setNewPassword(event.target.value)}
            minLength={8}
            required
          />
        </div>

        <div className={styles.field}>
          <label className={styles.label} htmlFor="confirm-password">
            确认新密码
          </label>
          <input
            id="confirm-password"
            className={styles.input}
            type="password"
            name="confirm"
            autoComplete="new-password"
            placeholder="再输入一次"
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
          {busy ? "保存中…" : hasPassword ? "更新密码" : "设置密码"}
        </button>

        {hasPassword && (
          <button
            type="button"
            className={styles.removeBtn}
            onClick={() => void removePassword()}
            disabled={busy}
          >
            移除密码
          </button>
        )}
      </form>
    </section>
  );
}
