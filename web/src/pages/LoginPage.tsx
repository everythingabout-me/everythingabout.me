import { Link, useSearchParams } from "react-router";
import { AuthForm } from "../components/AuthForm";
import styles from "../styles/auth.module.css";

export function LoginPage() {
  const [searchParams] = useSearchParams();
  const passwordMode = searchParams.get("mode") === "password";

  return (
    <div className={styles.authStack}>
      <title>{passwordMode ? "密码登录" : "登录 / 注册"} · Everything About Me</title>
      <section className={styles.card}>
        <div className={styles.eyebrow}>Sign in</div>
        <h1 className={styles.title}>{passwordMode ? "密码登录" : "登录 / 注册"}</h1>
        <AuthForm initialMode={passwordMode ? "password" : "code"} />
      </section>

      <Link
        className={styles.pageSwitch}
        to={passwordMode ? "/login" : "/login?mode=password"}
      >
        {passwordMode ? "改用邮箱验证码登录" : "用密码登录"}
      </Link>
    </div>
  );
}
