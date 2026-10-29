import type { ActivityItem } from "../api/client";
import { t, type MessageKey } from "../i18n";
import { formatRelativeTime } from "../i18n/format";
import styles from "../styles/auth.module.css";

const actionKeys: Record<string, MessageKey> = {
  put: "activity.action.put",
  delete: "activity.action.delete",
  move: "activity.action.move",
  copy: "activity.action.copy",
  upload: "activity.action.upload",
  upload_complete: "activity.action.upload",
  mkdir: "activity.action.mkdir",
  "event.create": "activity.action.eventCreate",
  "event.update": "activity.action.eventUpdate",
  "event.delete": "activity.action.eventDelete",
  "save_link.create": "activity.action.linkCreate",
  "save_link.revoke": "activity.action.linkRevoke",
  "connection.create": "activity.action.connectionCreate",
  "connection.revoke": "activity.action.connectionRevoke",
  cancel: "activity.action.cancel",
  retry: "activity.action.retry",
};

function actionLabel(action: string): string {
  const key = actionKeys[action];
  return key ? t(key) : action;
}

function sourceLabel(source: string): string {
  if (source === "Save link") return t("account.saveLinks.title");
  return source || t("account.activity.web");
}

export function ActivityList({ items }: { items: ActivityItem[] }) {
  return (
    <section>
      <h2 className={styles.sectionTitle}>{t("account.activity.title")}</h2>
      <p className={styles.sectionHint}>{t("account.activity.hint")}</p>

      {items.length === 0 ? (
        <p className={styles.footnote}>{t("account.activity.empty")}</p>
      ) : (
        <ul className={styles.activityList}>
          {items.map((item, index) => (
            <li className={styles.activityRow} key={`${item.createdAt}-${index}`}>
              <span className={styles.activityAction}>{actionLabel(item.action)}</span>
              <span className={styles.activityPath}>{item.path || item.title || "—"}</span>
              <span className={styles.activityMeta}>
                {sourceLabel(item.source)} · {formatRelativeTime(item.createdAt)}
                {item.status >= 400 ? ` · ${item.status}` : ""}
              </span>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
