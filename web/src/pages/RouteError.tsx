import { isRouteErrorResponse, useRouteError } from "react-router";
import { t } from "../i18n";
import styles from "../styles/auth.module.css";

/** 路由错误不一定是 Error：也可能是 react-router 的 ErrorResponse 或 loader 抛出的任意值。 */
function errorMessage(error: unknown): string {
  if (isRouteErrorResponse(error)) {
    const status = `${error.status} ${error.statusText}`.trim();
    const data =
      typeof error.data === "string" || typeof error.data === "number" ? String(error.data) : "";
    return data === "" ? status : `${status}: ${data}`;
  }
  if (error instanceof Error && error.message !== "") return error.message;
  if (typeof error === "string" && error !== "") return error;
  return t("error.unknown");
}

export function RouteError() {
  const error = useRouteError();
  const message = errorMessage(error);

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
