import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useLoaderData, useNavigate, useSearchParams } from "react-router";
import { ApiError, api, type AuthUser, type Crumb, type Item } from "../api/client";
import {
  ChevronRightIcon,
  CloseIcon,
  FolderIcon,
  GlyphFor,
  PlusIcon,
  RefreshIcon,
  SearchIcon,
  UploadIcon,
} from "../components/icons";
import { BrandMark } from "../components/BrandMark";
import { filesText, foldersText, resultsText, t } from "../i18n";
import { formatRelativeTime } from "../i18n/format";
import styles from "./DrivePage.module.css";

function joinPath(dir: string, name: string): string {
  return dir ? `${dir}/${name}` : name;
}

function formatSize(bytes: number): string {
  if (!bytes) return "—";
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let value = bytes / 1024;
  let index = 0;
  while (value >= 1024 && index < units.length - 1) {
    value /= 1024;
    index += 1;
  }
  return `${value.toFixed(value >= 100 || Number.isInteger(value) ? 0 : 1)} ${units[index]}`;
}

/** 根面包屑由前端命名，后端返回的「全部文件」只作为 API 数据。 */
function crumbName(crumb: Crumb): string {
  return crumb.path === "" ? t("drive.root") : crumb.name;
}

export function DrivePage() {
  const { user } = useLoaderData() as { user: AuthUser };
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const path = searchParams.get("path") ?? "";

  const [items, setItems] = useState<Item[]>([]);
  const [crumbTrail, setCrumbTrail] = useState<Crumb[]>([]);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [query, setQuery] = useState("");
  const [activeQuery, setActiveQuery] = useState("");
  const [creating, setCreating] = useState(false);
  const [newName, setNewName] = useState("");
  const [toast, setToast] = useState<{ text: string; kind: "info" | "error" } | null>(null);
  const [dragging, setDragging] = useState(false);

  const fileInput = useRef<HTMLInputElement>(null);
  const searchInput = useRef<HTMLInputElement>(null);
  const toastTimer = useRef<number | null>(null);
  const dragDepth = useRef(0);

  const showToast = useCallback(
    (text: string, kind: "info" | "error" = "info", sticky = false) => {
      setToast({ text, kind });
      if (toastTimer.current) window.clearTimeout(toastTimer.current);
      if (!sticky) toastTimer.current = window.setTimeout(() => setToast(null), 2600);
    },
    [],
  );

  const load = useCallback(
    async (target: string) => {
      setLoading(true);
      try {
        const data = await api.list(target);
        setItems(data.items);
        setCrumbTrail(data.breadcrumb);
        setActiveQuery("");
        setQuery("");
      } catch (error) {
        if (error instanceof ApiError && error.status === 401) {
          navigate("/login", { replace: true });
          return;
        }
        showToast(error instanceof Error ? error.message : t("error.loadFailed"), "error");
      } finally {
        setLoading(false);
      }
    },
    [navigate, showToast],
  );

  // 路径来自 URL（?path=），前进/后退和直接访问都能正确打开目录
  useEffect(() => {
    void load(path);
  }, [path, load]);

  const goTo = useCallback(
    (target: string) => {
      setActiveQuery("");
      setSearchParams(target ? { path: target } : {}, { replace: true });
    },
    [setSearchParams],
  );

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      const target = event.target as HTMLElement | null;
      const typing =
        target?.tagName === "INPUT" || target?.tagName === "TEXTAREA" || target?.isContentEditable;
      if (event.key === "/" && !typing) {
        event.preventDefault();
        searchInput.current?.focus();
      }
      if (event.key === "Escape") {
        setCreating(false);
        setToast(null);
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, []);

  const runSearch = async (keyword: string) => {
    const value = keyword.trim();
    if (!value) {
      void load(path);
      return;
    }
    setLoading(true);
    try {
      const data = await api.search(value);
      setItems(data.items);
      setActiveQuery(value);
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) {
        navigate("/login", { replace: true });
        return;
      }
      showToast(error instanceof Error ? error.message : t("error.searchFailed"), "error");
    } finally {
      setLoading(false);
    }
  };

  const upload = async (files: FileList | File[] | null) => {
    const list = files ? Array.from(files) : [];
    if (!list.length) return;
    setBusy(true);
    // COS SDK 只有上传时才需要，按需加载，首屏小很多
    const { default: COS } = await import("cos-js-sdk-v5");
    let done = 0;
    for (const file of list) {
      const name = file.name.normalize("NFC");
      const target = joinPath(path, name);
      // 默认不覆盖：同名文件要先确认。
      const exists = items.some((item) => !item.is_dir && item.name === name);
      if (exists && !window.confirm(t("drive.overwriteConfirm", { name }))) continue;
      try {
        showToast(t("drive.preparing", { name: file.name }), "info", true);
        const sign = await api.signUpload(target, file.size, exists);

        const cos = new COS({
          getAuthorization: (_options, callback) =>
            callback({
              TmpSecretId: sign.credentials.tmpSecretId,
              TmpSecretKey: sign.credentials.tmpSecretKey,
              SecurityToken: sign.credentials.sessionToken,
              StartTime: sign.credentials.startTime,
              ExpiredTime: sign.credentials.expiredTime,
            }),
        });

        await new Promise<void>((resolve, reject) => {
          cos.uploadFile(
            {
              Bucket: sign.bucket,
              Region: sign.region,
              Key: sign.key,
              Body: file,
              SliceSize: 5 * 1024 * 1024,
              onProgress: (info: { percent: number }) =>
                showToast(
                  `${list.length > 1 ? `${done + 1}/${list.length} · ` : ""}${file.name} ${Math.round(
                    info.percent * 100,
                  )}%`,
                  "info",
                  true,
                ),
            },
            (error) => (error ? reject(error) : resolve()),
          );
        });

        await api.confirmUpload(target, file.type || undefined, undefined, file.lastModified);
        done += 1;
      } catch (error) {
        showToast(
          `${file.name}: ${t("drive.uploadFailed", {
            error: error instanceof Error ? error.message : t("error.unknown"),
          })}`,
          "error",
        );
      }
    }
    setBusy(false);
    if (fileInput.current) fileInput.current.value = "";
    if (done) {
      showToast(
        done === 1
          ? t("drive.uploadCompleted")
          : t("drive.uploadCompletedMany", { files: filesText(done) }),
      );
    }
    await load(path);
  };

  const createFolder = async () => {
    const name = newName.trim().normalize("NFC");
    if (!name) {
      setCreating(false);
      return;
    }
    try {
      await api.mkdir(joinPath(path, name));
      setCreating(false);
      setNewName("");
      await load(path);
    } catch (error) {
      showToast(error instanceof Error ? error.message : t("error.saveFailed"), "error");
    }
  };

  const totalSize = items.reduce((sum, item) => (item.is_dir ? sum : sum + Number(item.size)), 0);
  const fileCount = items.filter((item) => !item.is_dir).length;
  const folderCount = items.filter((item) => item.is_dir).length;
  const currentCrumb = crumbTrail[crumbTrail.length - 1];
  const parentCrumbs = crumbTrail.slice(0, -1);
  const currentName = currentCrumb ? crumbName(currentCrumb) : t("drive.root");

  const onDrop = (event: React.DragEvent) => {
    event.preventDefault();
    dragDepth.current = 0;
    setDragging(false);
    void upload(event.dataTransfer.files);
  };

  return (
    <div
      className={styles.page}
      onDragEnter={(event) => {
        if (!event.dataTransfer.types.includes("Files")) return;
        dragDepth.current += 1;
        setDragging(true);
      }}
      onDragOver={(event) => event.preventDefault()}
      onDragLeave={() => {
        dragDepth.current -= 1;
        if (dragDepth.current <= 0) setDragging(false);
      }}
      onDrop={onDrop}
    >
      <title>{t("app.name")}</title>
      <header className={styles.topbar}>
        <div className={styles.topbarInner}>
          <Link className={styles.brand} to="/" title={t("nav.home")}>
            <BrandMark className={styles.brandMark} />
            <span className={styles.brandText}>{t("app.name")}</span>
          </Link>

          <div className={styles.topSpacer} />

          <div className={styles.search}>
            <span className={styles.searchIcon}>
              <SearchIcon size={14} />
            </span>
            <input
              ref={searchInput}
              className={styles.searchInput}
              placeholder={t("drive.searchPlaceholder")}
              value={query}
              onChange={(event) => {
                setQuery(event.target.value);
                if (!event.target.value && activeQuery) void load(path);
              }}
              onKeyDown={(event) => {
                if (event.key === "Enter") void runSearch(query);
                if (event.key === "Escape") {
                  setQuery("");
                  (event.target as HTMLInputElement).blur();
                }
              }}
            />
            <span className={styles.kbd}>/</span>
          </div>

          <Link
            className={styles.accountBtn}
            to="/account"
            title={t("nav.accountSettings")}
            aria-label={t("nav.accountSettingsFor", { email: user.email })}
          >
            <span className={styles.avatar}>{user.email.slice(0, 1).toUpperCase()}</span>
            <span className={styles.accountEmail}>{user.email}</span>
          </Link>

          <button
            className={styles.primaryBtn}
            onClick={() => fileInput.current?.click()}
            disabled={busy}
          >
            <UploadIcon size={14} />
            {busy ? t("drive.uploading") : t("drive.upload")}
          </button>

          <input
            ref={fileInput}
            type="file"
            multiple
            hidden
            onChange={(event) => void upload(event.target.files)}
          />
        </div>
      </header>

      <main className={styles.main}>
        <div className={styles.masthead}>
          <div className={styles.mastheadMain}>
            {activeQuery ? (
              <>
                <h1 className={styles.pageTitle}>{t("drive.results")}</h1>
                <p className={styles.stats}>
                  {t("drive.resultsFor", {
                    query: activeQuery,
                    results: resultsText(items.length),
                  })}
                  <button className={styles.clearLink} onClick={() => void load(path)}>
                    {t("drive.clear")}
                  </button>
                </p>
              </>
            ) : (
              <>
                {parentCrumbs.length > 0 && (
                  <nav className={styles.breadcrumbs} aria-label={t("drive.pathLabel")}>
                    {parentCrumbs.map((crumb, index) => (
                      <span key={crumb.path || "root"} style={{ display: "contents" }}>
                        {index > 0 && (
                          <span className={styles.breadcrumbSep}>
                            <ChevronRightIcon size={12} />
                          </span>
                        )}
                        <button className={styles.crumbLink} onClick={() => goTo(crumb.path)}>
                          {crumbName(crumb)}
                        </button>
                      </span>
                    ))}
                  </nav>
                )}
                <h1 className={styles.pageTitle}>{currentName}</h1>
                <p className={styles.stats}>
                  {foldersText(folderCount)} · {filesText(fileCount)}
                  {totalSize > 0 && <span> · {formatSize(totalSize)}</span>}
                </p>
              </>
            )}
          </div>

          <div className={styles.mastheadActions}>
            <button
              className={styles.secondaryBtn}
              onClick={() => {
                setCreating(true);
                setNewName("");
              }}
              disabled={Boolean(activeQuery)}
            >
              <PlusIcon size={14} />
              {t("drive.newFolder")}
            </button>
            <button
              className={styles.ghostBtn}
              onClick={() => void load(path)}
              title={t("common.refresh")}
              aria-label={t("common.refresh")}
            >
              <RefreshIcon size={16} />
            </button>
          </div>
        </div>

        <section className={styles.panel} aria-busy={loading}>
          {creating && (
            <div className={styles.newRow}>
              <input
                className={styles.newInput}
                autoFocus
                placeholder={t("drive.newFolderPlaceholder")}
                value={newName}
                onChange={(event) => setNewName(event.target.value)}
                onKeyDown={(event) => {
                  if (event.key === "Enter") void createFolder();
                  if (event.key === "Escape") setCreating(false);
                }}
              />
              <button className={styles.textBtn} onClick={() => void createFolder()}>
                {t("drive.create")}
              </button>
              <button className={styles.textBtn} onClick={() => setCreating(false)}>
                {t("common.cancel")}
              </button>
            </div>
          )}

          {loading &&
            Array.from({ length: 4 }).map((_, index) => (
              <div key={index} className={styles.skeletonRow}>
                <div className={styles.skBar} style={{ width: 16, height: 16, borderRadius: 4 }} />
                <div className={styles.skBar} style={{ width: `${38 + index * 11}%` }} />
                <div className={styles.skBar} />
                <div className={styles.skBar} />
              </div>
            ))}

          {!loading &&
            items.map((item) => (
              <div key={item.id} className={styles.row}>
                <span className={styles.glyph}>
                  <GlyphFor name={item.name} mime={item.mime} isDir={item.is_dir} />
                </span>

                <div className={styles.nameCell}>
                  {item.is_dir ? (
                    <button
                      className={`${styles.name} ${styles.nameDir}`}
                      onClick={() => goTo(item.path)}
                      title={item.name}
                    >
                      {item.name}
                    </button>
                  ) : (
                    <a
                      className={styles.name}
                      href={api.downloadHref(item.path)}
                      target="_blank"
                      rel="noreferrer"
                      title={item.name}
                    >
                      <span className={styles.nameLink}>{item.name}</span>
                    </a>
                  )}
                  {activeQuery && item.parent && (
                    <span className={styles.pathHint}>{item.parent}</span>
                  )}
                </div>

                <span className={styles.size}>
                  {item.is_dir ? "" : formatSize(Number(item.size))}
                </span>
                <span className={styles.time}>{formatRelativeTime(item.updated_at)}</span>
              </div>
            ))}

          {!loading && items.length === 0 && (
            <div className={styles.empty}>
              <span className={styles.emptyIcon}>
                {activeQuery ? <SearchIcon size={20} /> : <FolderIcon size={20} />}
              </span>
              <span className={styles.emptyTitle}>
                {activeQuery ? t("drive.emptySearchTitle") : t("drive.emptyFolderTitle")}
              </span>
              {activeQuery && (
                <span className={styles.emptyText}>{t("drive.emptySearchHint")}</span>
              )}
            </div>
          )}
        </section>

        {!loading && items.length > 0 && (fileCount > 0 || totalSize > 0) && (
          <footer className={styles.footer}>
            {fileCount > 0 && <span>{filesText(fileCount)}</span>}
            {fileCount > 0 && totalSize > 0 && <span>·</span>}
            {totalSize > 0 && <span>{formatSize(totalSize)}</span>}
          </footer>
        )}
      </main>

      {dragging && (
        <div className={styles.dropOverlay}>
          <div className={styles.dropInner}>
            <UploadIcon size={20} />
            <span>{t("drive.dropToUpload", { target: currentName })}</span>
          </div>
        </div>
      )}

      {toast && (
        <div className={`${styles.toast} ${toast.kind === "error" ? styles.toastError : ""}`}>
          <span>{toast.text}</span>
          {toast.kind === "error" && (
            <button
              className={styles.ghostBtn}
              style={{ width: 20, height: 20, color: "inherit" }}
              onClick={() => setToast(null)}
              aria-label={t("common.close")}
            >
              <CloseIcon size={12} />
            </button>
          )}
        </div>
      )}

    </div>
  );
}
