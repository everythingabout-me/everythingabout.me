import { useEffect, useRef, useState } from "react";
import { api, type Item } from "../api/client";
import { ChevronRightIcon, FolderIcon } from "./icons";
import { t } from "../i18n";
import styles from "./TransferDialog.module.css";

export type TransferMode = "move" | "copy";

/** Root-safe comparison of two current paths: "/Photos/" and "Photos" are the same folder. */
function cleanPath(path: string): string {
  return path.replace(/^\/+/, "").replace(/\/+$/, "");
}

/**
 * Move or copy one selected entry into another folder. The browser walks the tree through the
 * ordinary listing endpoint, so a destination is always a folder the current principal can read;
 * the server rechecks write access and refuses a moved folder's own subtree.
 */
export function TransferDialog({
  mode,
  item,
  onCancel,
  onConfirm,
}: {
  mode: TransferMode;
  item: Item;
  onCancel: () => void;
  onConfirm: (destination: string) => Promise<void>;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const submitting = useRef(false);
  // A move starts beside the source, where the destination usually is. A copy starts at the
  // root because copying into the source's own parent always collides with its name.
  const [browse, setBrowse] = useState(mode === "move" ? item.parentPath || "/" : "/");
  const [folders, setFolders] = useState<Item[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const element = dialog.current;
    element?.showModal();
    return () => element?.close();
  }, []);

  useEffect(() => {
    let current = true;
    setLoading(true);
    setLoadError(null);
    api
      .list(browse)
      .then((result) => {
        if (!current) return;
        setFolders(result.items.filter((entry) => entry.kind === "folder"));
      })
      .catch((caught: unknown) => {
        if (!current) return;
        setLoadError(caught instanceof Error ? caught.message : t("error.loadFailed"));
      })
      .finally(() => {
        if (current) setLoading(false);
      });
    return () => {
      current = false;
    };
  }, [browse]);

  const ownPath = cleanPath(item.path);
  const destination = cleanPath(browse);
  // A folder cannot move or copy into itself or below itself. The current parent is also
  // refused: a move there is a no-op, and a copy there collides with the source's own name
  // because the API never renames on conflict.
  const blocked =
    item.kind === "folder" && (destination === ownPath || destination.startsWith(`${ownPath}/`))
      ? t("drive.destinationInside", { name: item.name })
      : destination === cleanPath(item.parentPath ?? "/")
        ? t("drive.destinationSameParent")
        : null;

  const crumbs = [
    { name: t("drive.root"), path: "/" },
    ...browse
      .split("/")
      .filter(Boolean)
      .map((segment, index, segments) => ({
        name: segment,
        path: `/${segments.slice(0, index + 1).join("/")}`,
      })),
  ];

  const confirm = async () => {
    if (submitting.current || blocked) return;
    submitting.current = true;
    setBusy(true);
    setError(null);
    try {
      await onConfirm(browse);
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : t("drive.transferFailed"));
    } finally {
      submitting.current = false;
      setBusy(false);
    }
  };

  return (
    <dialog
      ref={dialog}
      className={styles.dialog}
      aria-labelledby="transfer-title"
      onCancel={(event) => {
        event.preventDefault();
        if (!submitting.current) onCancel();
      }}
    >
      <h2 id="transfer-title" className={styles.title}>
        {t(mode === "move" ? "drive.moveTitle" : "drive.copyTitle", { name: item.name })}
      </h2>
      <p className={styles.hint}>{t("drive.chooseFolder")}</p>

      <nav className={styles.crumbs} aria-label={t("drive.pathLabel")}>
        {crumbs.map((crumb, index) => (
          <span key={crumb.path} className={styles.crumbItem}>
            {index > 0 && (
              <span className={styles.crumbSep}>
                <ChevronRightIcon size={12} />
              </span>
            )}
            {index === crumbs.length - 1 ? (
              <span className={styles.crumbCurrent}>{crumb.name}</span>
            ) : (
              <button className={styles.crumb} onClick={() => setBrowse(crumb.path)} disabled={busy}>
                {crumb.name}
              </button>
            )}
          </span>
        ))}
      </nav>

      <div className={styles.list} aria-busy={loading}>
        {loading ? (
          <span className={styles.placeholder}>{t("common.loading")}</span>
        ) : loadError ? (
          <span className={styles.error} role="alert">
            {loadError}
          </span>
        ) : folders.length === 0 ? (
          <span className={styles.placeholder}>{t("drive.noSubfolders")}</span>
        ) : (
          folders.map((folder) => (
            <button
              key={folder.path}
              className={styles.folder}
              onClick={() => setBrowse(folder.path)}
              disabled={busy}
            >
              <FolderIcon size={15} />
              <span className={styles.folderName}>{folder.name}</span>
              <ChevronRightIcon size={14} />
            </button>
          ))
        )}
      </div>

      {blocked && <p className={styles.warning}>{blocked}</p>}
      {error && (
        <p className={styles.error} role="alert">
          {error}
        </p>
      )}

      <div className={styles.actions}>
        <button className={styles.cancel} disabled={busy} onClick={onCancel}>
          {t("common.cancel")}
        </button>
        <button className={styles.primary} disabled={busy || blocked !== null} onClick={() => void confirm()}>
          {busy
            ? t(mode === "move" ? "drive.moving" : "drive.copying")
            : t(mode === "move" ? "drive.moveHere" : "drive.copyHere")}
        </button>
      </div>
    </dialog>
  );
}
