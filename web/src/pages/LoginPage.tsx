import { useSearchParams } from "react-router";
import { AuthForm } from "../components/AuthForm";
import { t } from "../i18n";
import styles from "../styles/auth.module.css";

export function LoginPage() {
  const [searchParams] = useSearchParams();
  // 密码登录不再显示入口，只保留 /login?mode=password 直达
  const passwordMode = searchParams.get("mode") === "password";

  return (
    <>
      <title>{t(passwordMode ? "meta.loginPasswordTitle" : "meta.loginCodeTitle")}</title>
      <section className={styles.card}>
        <h1 className={styles.title}>
          {t(passwordMode ? "auth.titlePassword" : "auth.titleCode")}
        </h1>
        <AuthForm initialMode={passwordMode ? "password" : "code"} />
      </section>
    </>
  );
}
