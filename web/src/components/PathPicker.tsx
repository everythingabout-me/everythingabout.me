import { Fragment, useEffect, useRef, useState } from "react";
import { api, type Item } from "../api/client";
import { t } from "../i18n";
import { ChevronDownIcon, ChevronRightIcon, FolderIcon } from "./icons";
import styles from "./PathPicker.module.css";

/**
 * Pick the folder a connection may reach. The whole drive (empty path) is the default; the
 * control browses with the same /api/fs/list the file page uses, so no extra endpoint exists
 * only for this picker.
 */
export function PathPicker({
  value,
  onChange,
}: {
  value: string;
  onChange: (path: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const [dir, setDir] = useState(value);
  const [items, setItems] = useState<Item[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const wrap = useRef<HTMLDivElement>(null);

  // Close on an outside click or Escape, like the other transient surfaces on the page.
  useEffect(() => {
    if (!open) return;
    function onPointerDown(event: PointerEvent) {
      if (wrap.current && !wrap.current.contains(event.target as Node)) setOpen(false);
    }
    function onKeyDown(event: KeyboardEvent) {
      if (event.key === "Escape") setOpen(false);
    }
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [open]);

  // Fetch one level at a time, so a large drive never loads all at once.
  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    setLoading(true);
    setError(null);
    api
      .list(dir)
      .then((result) => {
        if (cancelled) return;
        setItems(
          result.items.filter((item) => item.is_dir).sort((left, right) => left.name.localeCompare(right.name)),
        );
      })
      .catch((caught) => {
        if (cancelled) return;
        setItems([]);
        setError(caught instanceof Error ? caught.message : t("connect.pathError"));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [open, dir]);

  const crumbs = dir ? dir.split("/") : [];

  return (
    <div className={styles.wrap} ref={wrap}>
      <button
        className={`${styles.trigger} ${open ? styles.triggerOpen : ""}`}
        type="button"
        aria-haspopup="dialog"
        aria-expanded={open}
        onClick={() => {
          setDir(value);
          setOpen((current) => !current);
        }}
      >
        <FolderIcon size={14} className={styles.triggerIcon} />
        <span className={styles.triggerValue}>{value || t("connect.wholeDrive")}</span>
        <ChevronDownIcon size={12} className={styles.triggerChevron} />
      </button>

      {open && (
        <div className={styles.popover} role="dialog" aria-label={t("connect.path")}>
          <div className={styles.crumbs}>
            <button
              className={`${styles.crumb} ${crumbs.length === 0 ? styles.crumbHere : ""}`}
              type="button"
              onClick={() => setDir("")}
            >
              {t("connect.wholeDrive")}
            </button>
            {crumbs.map((name, index) => {
              const path = crumbs.slice(0, index + 1).join("/");
              const here = index === crumbs.length - 1;
              return (
                <Fragment key={path}>
                  <ChevronRightIcon size={11} className={styles.crumbSep} />
                  <button
                    className={`${styles.crumb} ${here ? styles.crumbHere : ""}`}
                    type="button"
                    title={name}
                    onClick={() => setDir(path)}
                  >
                    {name}
                  </button>
                </Fragment>
              );
            })}
          </div>

          <div className={styles.folders}>
            {loading ? (
              <p className={styles.state}>{t("connect.pathLoading")}</p>
            ) : error ? (
              <p className={styles.state}>{error}</p>
            ) : items.length === 0 ? (
              <p className={styles.state}>{t("connect.pathEmpty")}</p>
            ) : (
              items.map((item) => (
                <button
                  key={item.id}
                  className={styles.folder}
                  type="button"
                  title={item.name}
                  onClick={() => setDir(item.path)}
                >
                  <FolderIcon size={14} className={styles.folderIcon} />
                  <span className={styles.folderName}>{item.name}</span>
                  <ChevronRightIcon size={12} className={styles.folderChevron} />
                </button>
              ))
            )}
          </div>

          <div className={styles.foot}>
            <span className={styles.footPath} title={dir || t("connect.wholeDrive")}>
              {dir || t("connect.wholeDrive")}
            </span>
            <button
              className={styles.choose}
              type="button"
              onClick={() => {
                onChange(dir);
                setOpen(false);
              }}
            >
              {dir ? t("connect.pathChoose") : t("connect.pathChooseDrive")}
            </button>
          </div>
        </div>
      )}
    </div>
  );
}
