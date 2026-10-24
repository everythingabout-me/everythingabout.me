import { isRouteErrorResponse, useRouteError } from "react-router";
import styles from "../styles/auth.module.css";

export function RouteError() {
  const error = useRouteError();

  let message = "未知错误";
  if (isRouteErrorResponse(error)) {
    message = `${error.status} ${error.statusText}`;
    if (error.data) message += `：${String(error.data)}`;
  } else if (error instanceof Error) {
    message = error.message;
  }

  return (
    <div className={styles.page}>
      <main className={styles.main}>
        <section className={styles.card}>
          <div className={styles.eyebrow}>Error</div>
          <h1 className={styles.title}>页面出错了</h1>
          <p className={styles.subtitle}>{message}</p>
          <div className={styles.form}>
            <button
              className={styles.submit}
              type="button"
              onClick={() => window.location.assign("/")}
            >
              回到首页
            </button>
          </div>
        </section>
      </main>
    </div>
  );
}
