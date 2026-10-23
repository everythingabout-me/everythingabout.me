import { redirect } from "next/navigation";
import { getSessionUser } from "@/lib/auth";
import { AuthForm } from "../auth-form";
import styles from "../auth.module.css";

export const dynamic = "force-dynamic";

export const metadata = {
  title: "登录 / 注册 · Everything About Me",
};

export default async function LoginPage({
  searchParams,
}: {
  searchParams: Promise<{ mode?: string | string[] }>;
}) {
  if (await getSessionUser()) redirect("/");

  const params = await searchParams;
  const rawMode = Array.isArray(params.mode) ? params.mode[0] : params.mode;
  const passwordMode = rawMode === "password";

  return (
    <div className={styles.authStack}>
      <section className={styles.card}>
        <div className={styles.eyebrow}>Sign in</div>
        <h1 className={styles.title}>
          {passwordMode ? "密码登录" : "登录 / 注册"}
        </h1>
        <AuthForm initialMode={passwordMode ? "password" : "code"} />
      </section>

      <a
        className={styles.pageSwitch}
        href={passwordMode ? "/login" : "/login?mode=password"}
      >
        {passwordMode ? "改用邮箱验证码登录" : "用密码登录"}
      </a>
    </div>
  );
}
