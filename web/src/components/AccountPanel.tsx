import { useState } from "react";
import { useNavigate } from "react-router";
import { api, type AuthUser } from "../api/client";
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
          <h1 className={styles.title}>账户</h1>
          <p className={styles.subtitle}>邮箱就是你的唯一标识；默认用验证码登录，密码可选。</p>
        </div>
        <button className={styles.ghostBtn} onClick={() => void logout()} disabled={busy}>
          退出登录
        </button>
      </div>

      <dl className={styles.metaList}>
        <div className={styles.metaRow}>
          <dt className={styles.metaLabel}>登录邮箱</dt>
          <dd className={styles.metaValue}>{email}</dd>
        </div>
        <div className={styles.metaRow}>
          <dt className={styles.metaLabel}>注册时间</dt>
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
            检测到 {legacyCount} 个旧文件还没有归属。在服务器上运行：
            <code className={styles.command}>
              eam claim --email {email}
            </code>
            即可把它们认领到这个账号。
          </p>
        </>
      )}
    </section>
  );
}
