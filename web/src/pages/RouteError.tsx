import { isRouteErrorResponse, useRouteError } from "react-router";
import { t } from "../i18n";
import styles from "../styles/auth.module.css";

export function RouteError() {
  const error = useRouteError();

  let message = t("error.unknown");
  if (isRouteErrorResponse(error)) {
    message = `${error.status} ${error.statusText}`;
    if (error.data) message += `: ${String(error.data)}`;
  } else if (error instanceof Error) {
    message = error.message;
  }

  return (
    <div className={styles.page}>
      <title>{`${t("error.title")} · ${t("app.name")}`}</title>
      <main className={styles.main}>
        <section className={styles.card}>
          <div className={styles.eyebrow}>{t("error.eyebrow")}</div>
          <h1 className={styles.title}>{t("error.title")}</h1>
          <p className={styles.subtitle}>{message}</p>
          <div className={styles.form}>
            <button
              className={styles.submit}
              type="button"
              onClick={() => window.location.assign("/")}
            >
              {t("error.backHome")}
            </button>
          </div>
        </section>
      </main>
    </div>
  );
}
