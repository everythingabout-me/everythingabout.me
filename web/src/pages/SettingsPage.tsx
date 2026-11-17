import { NavLink, Outlet, useLoaderData, useLocation } from "react-router";
import type { AccountSummary } from "../api/client";
import { AppHeader } from "../components/AppHeader";
import { t, type MessageKey } from "../i18n";
import styles from "./SettingsPage.module.css";

/**
 * The settings pane's data: the account summary, including the signed-in user. It is loaded
 * once for the whole page, so switching sections does not re-read the account.
 */
export interface SettingsContext {
  summary: AccountSummary;
}

interface Section {
  to: string;
  end?: boolean;
  title: MessageKey;
  lead: MessageKey;
}

/**
 * Settings is one page with its own left navigation: the list is the section index, the pane on
 * the right is the section itself. Each section is a real URL under /settings, so it can be
 * linked to and the back button works, while the layout and the pane header are written once
 * here instead of each section repeating them.
 */
const SETTINGS_SECTIONS: Section[] = [
  { to: "/settings", end: true, title: "settings.general.title", lead: "settings.general.lead" },
  { to: "/settings/connect", title: "settings.connect.title", lead: "settings.connect.lead" },
  { to: "/settings/storage", title: "settings.storage.title", lead: "settings.storage.lead" },
];

export function SettingsPage() {
  const { summary } = useLoaderData() as { summary: AccountSummary };
  const { pathname } = useLocation();
  const section = SETTINGS_SECTIONS.find((item) => item.to === pathname) ?? SETTINGS_SECTIONS[0];

  return (
    <div className={styles.page}>
      <title>{`${t(section.title)} · ${t("meta.settingsTitle")}`}</title>
      <AppHeader user={summary.user} />
      <main className={styles.main}>
        <div className={styles.layout}>
          <aside className={styles.sidebar}>
            <h1 className={styles.heading}>{t("settings.title")}</h1>
            <nav className={styles.nav} aria-label={t("settings.sections")}>
              {SETTINGS_SECTIONS.map((item) => (
                <NavLink
                  key={item.to}
                  to={item.to}
                  end={item.end}
                  className={({ isActive }) =>
                    isActive ? `${styles.navItem} ${styles.navItemActive}` : styles.navItem
                  }
                >
                  {t(item.title)}
                </NavLink>
              ))}
            </nav>
          </aside>

          <section className={styles.pane}>
            <header className={styles.paneHead}>
              <h2 className={styles.paneTitle}>{t(section.title)}</h2>
              <p className={styles.paneLead}>{t(section.lead)}</p>
            </header>
            <Outlet context={{ summary }} />
          </section>
        </div>
      </main>
    </div>
  );
}
