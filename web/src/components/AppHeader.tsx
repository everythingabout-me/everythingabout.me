import { useEffect, useRef } from "react";
import { Link, useLocation } from "react-router";
import type { AuthUser } from "../api/client";
import { t } from "../i18n";
import { BrandMark } from "./BrandMark";
import { SearchIcon, UploadIcon } from "./icons";
import styles from "./AppHeader.module.css";

/** The search box is owned by the page; the header only lays it out and wires the shortcuts. */
export interface HeaderSearch {
  value: string;
  onChange: (value: string) => void;
  onSubmit: () => void;
}

/**
 * The one top bar for every signed-in page: brand, primary navigation (with the current page
 * highlighted) and the account chip. Pages may add a search box and one primary action, so the
 * file browser keeps its tools without growing a second header.
 */
export function AppHeader({
  user,
  search,
  upload,
}: {
  user: AuthUser;
  search?: HeaderSearch;
  upload?: { count: number; onClick: () => void };
}) {
  const { pathname } = useLocation();
  const searchInput = useRef<HTMLInputElement>(null);
  const hasSearch = Boolean(search);

  // "/" focuses search from anywhere, except while typing in a field.
  useEffect(() => {
    if (!hasSearch) return;
    const onKeyDown = (event: KeyboardEvent) => {
      const target = event.target as HTMLElement | null;
      const typing =
        target?.tagName === "INPUT" || target?.tagName === "TEXTAREA" || target?.isContentEditable;
      if (event.key === "/" && !typing) {
        event.preventDefault();
        searchInput.current?.focus();
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [hasSearch]);

  const homeActive = pathname === "/";
  const filesActive = pathname === "/files" || pathname.startsWith("/files/");

  return (
    <header className={styles.topbar}>
      <div className={`${styles.topbarInner} ${hasSearch ? styles.withSearch : ""}`}>
        <Link className={styles.brand} to="/" title={t("nav.home")}>
          <BrandMark className={styles.brandMark} />
          <span className={styles.brandText}>{t("app.name")}</span>
        </Link>

        <nav className={styles.nav} aria-label={t("nav.primary")}>
          <Link
            className={`${styles.navLink} ${homeActive ? styles.navLinkActive : ""}`}
            to="/"
            aria-current={homeActive ? "page" : undefined}
          >
            {t("nav.stream")}
          </Link>
          <Link
            className={`${styles.navLink} ${filesActive ? styles.navLinkActive : ""}`}
            to="/files"
            aria-current={filesActive ? "page" : undefined}
          >
            {t("nav.files")}
          </Link>
        </nav>

        <div className={styles.spacer} />

        {search && (
          <div className={styles.search}>
            <span className={styles.searchIcon}>
              <SearchIcon size={14} />
            </span>
            <input
              ref={searchInput}
              className={styles.searchInput}
              placeholder={t("drive.searchPlaceholder")}
              aria-label={t("drive.searchPlaceholder")}
              value={search.value}
              onChange={(event) => search.onChange(event.target.value)}
              onKeyDown={(event) => {
                if (event.key === "Enter") search.onSubmit();
                if (event.key === "Escape") {
                  search.onChange("");
                  event.currentTarget.blur();
                }
              }}
            />
            <span className={styles.kbd}>/</span>
          </div>
        )}

        <Link
          className={styles.account}
          to="/account"
          title={t("nav.accountSettings")}
          aria-label={t("nav.accountSettingsFor", { email: user.email })}
        >
          <span className={styles.avatar}>{user.email.slice(0, 1).toUpperCase()}</span>
          <span className={styles.email}>{user.email}</span>
        </Link>

        {upload && (
          <button className={styles.primaryBtn} onClick={upload.onClick} title={t("drive.upload")}>
            <UploadIcon size={14} />
            <span className={styles.uploadLabel}>{t("drive.upload")}</span>
            {upload.count > 0 && <span className={styles.uploadBadge}>{upload.count}</span>}
          </button>
        )}
      </div>
    </header>
  );
}
