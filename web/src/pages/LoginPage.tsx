import { Link, useSearchParams } from "react-router";
import { AuthForm } from "../components/AuthForm";
import { t } from "../i18n";
import styles from "../styles/auth.module.css";

export function LoginPage() {
  const [searchParams] = useSearchParams();
  const passwordMode = searchParams.get("mode") === "password";

  return (
    <div className={styles.authStack}>
      <title>{t(passwordMode ? "meta.loginPasswordTitle" : "meta.loginCodeTitle")}</title>
      <section className={styles.card}>
        <div className={styles.eyebrow}>{t("auth.signInEyebrow")}</div>
        <h1 className={styles.title}>
          {t(passwordMode ? "auth.titlePassword" : "auth.titleCode")}
        </h1>
        <AuthForm initialMode={passwordMode ? "password" : "code"} />
      </section>

      <Link
        className={styles.pageSwitch}
        to={passwordMode ? "/login" : "/login?mode=password"}
      >
        {t(passwordMode ? "auth.switchToCode" : "auth.switchToPassword")}
      </Link>
    </div>
  );
}
