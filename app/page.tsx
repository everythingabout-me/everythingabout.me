"use client";

import COS from "cos-js-sdk-v5";
import { useCallback, useEffect, useRef, useState } from "react";
import {
  CloseIcon,
  DownloadIcon,
  FolderIcon,
  GlyphFor,
  PlusIcon,
  RefreshIcon,
  SearchIcon,
  TrashIcon,
  UploadIcon,
  ChevronRightIcon,
} from "@/components/icons";
import styles from "./page.module.css";

interface Item {
  id: string;
  path: string;
  parent: string;
  name: string;
  is_dir: boolean;
  size: number;
  mime: string | null;
  updated_at: string;
  meta: Record<string, unknown>;
}

interface Crumb {
  name: string;
  path: string;
}

interface ApiResponse<T> {
  ok: boolean;
  data?: T;
  error?: string;
}

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
  return `${value.toFixed(value >= 10 ? 0 : 1)} ${units[index]}`;
}

function formatTime(iso: string): string {
  const then = new Date(iso).getTime();
  const diff = Date.now() - then;
  const minute = 60_000;
  const hour = 60 * minute;
  const day = 24 * hour;
  if (diff < minute) return "刚刚";
  if (diff < hour) return `${Math.floor(diff / minute)} 分钟前`;
  if (diff < day) return `${Math.floor(diff / hour)} 小时前`;
  if (diff < 7 * day) return `${Math.floor(diff / day)} 天前`;
  const date = new Date(iso);
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, "0")}-${String(
    date.getDate(),
  ).padStart(2, "0")}`;
}

export default function Page() {
  const [path, setPath] = useState("");
  const [items, setItems] = useState<Item[]>([]);
  const [crumbs, setCrumbs] = useState<Crumb[]>([{ name: "全部文件", path: "" }]);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [query, setQuery] = useState("");
  const [activeQuery, setActiveQuery] = useState("");
  const [creating, setCreating] = useState(false);
  const [newName, setNewName] = useState("");
  const [pendingDelete, setPendingDelete] = useState<Item | null>(null);
  const [toast, setToast] = useState<{ text: string; kind: "info" | "error" } | null>(null);
  const [dragging, setDragging] = useState(false);

  const fileInput = useRef<HTMLInputElement>(null);
  const searchInput = useRef<HTMLInputElement>(null);
  const toastTimer = useRef<number | null>(null);
  const dragDepth = useRef(0);

  const showToast = useCallback((text: string, kind: "info" | "error" = "info", sticky = false) => {
    setToast({ text, kind });
    if (toastTimer.current) window.clearTimeout(toastTimer.current);
    if (!sticky) toastTimer.current = window.setTimeout(() => setToast(null), 2600);
  }, []);

  const load = useCallback(
    async (target: string) => {
      setLoading(true);
      try {
        const response = await fetch(`/api/fs/list?path=${encodeURIComponent(target)}`);
        const payload = (await response.json()) as ApiResponse<{
          path: string;
          items: Item[];
          breadcrumb: Crumb[];
        }>;
        if (!payload.ok || !payload.data) throw new Error(payload.error ?? "加载失败");
        setItems(payload.data.items);
        setCrumbs(payload.data.breadcrumb);
        setPath(payload.data.path);
        setActiveQuery("");
        setQuery("");
        window.history.replaceState(
          null,
          "",
          payload.data.path ? `?path=${encodeURIComponent(payload.data.path)}` : "/",
        );
      } catch (error) {
        showToast(error instanceof Error ? error.message : "加载失败", "error");
      } finally {
        setLoading(false);
      }
    },
    [showToast],
  );

  useEffect(() => {
    const initial = new URLSearchParams(window.location.search).get("path") ?? "";
    void load(initial);
  }, [load]);

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
        setPendingDelete(null);
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
      const response = await fetch(`/api/fs/search?q=${encodeURIComponent(value)}`);
      const payload = (await response.json()) as ApiResponse<{ items: Item[] }>;
      if (!payload.ok || !payload.data) throw new Error(payload.error ?? "搜索失败");
      setItems(payload.data.items);
      setActiveQuery(value);
    } catch (error) {
      showToast(error instanceof Error ? error.message : "搜索失败", "error");
    } finally {
      setLoading(false);
    }
  };

  const upload = async (files: FileList | File[] | null) => {
    const list = files ? Array.from(files) : [];
    if (!list.length) return;
    setBusy(true);
    let done = 0;
    for (const file of list) {
      const target = joinPath(path, file.name.normalize("NFC"));
      try {
        showToast(`准备 ${file.name}`, "info", true);
        const signResponse = await fetch("/api/fs/upload/sign", {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ path: target, size: file.size }),
        });
        const sign = (await signResponse.json()) as ApiResponse<{
          bucket: string;
          region: string;
          key: string;
          credentials: {
            tmpSecretId: string;
            tmpSecretKey: string;
            sessionToken: string;
            startTime: number;
            expiredTime: number;
          };
        }>;
        if (!sign.ok || !sign.data) throw new Error(sign.error ?? "无法获取上传凭证");

        const cos = new COS({
          getAuthorization: (_options: unknown, callback: (data: unknown) => void) =>
            callback({
              TmpSecretId: sign.data!.credentials.tmpSecretId,
              TmpSecretKey: sign.data!.credentials.tmpSecretKey,
              SecurityToken: sign.data!.credentials.sessionToken,
              StartTime: sign.data!.credentials.startTime,
              ExpiredTime: sign.data!.credentials.expiredTime,
            }),
        });

        await new Promise((resolve, reject) => {
          cos.uploadFile(
            {
              Bucket: sign.data!.bucket,
              Region: sign.data!.region,
              Key: sign.data!.key,
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
            (error: unknown) => (error ? reject(error) : resolve(null)),
          );
        });

        const confirmResponse = await fetch("/api/fs/upload/confirm", {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ path: target, mime: file.type || undefined }),
        });
        const confirm = (await confirmResponse.json()) as ApiResponse<unknown>;
        if (!confirm.ok) throw new Error(confirm.error ?? "落库失败");
        done += 1;
      } catch (error) {
        showToast(
          `${file.name} 上传失败：${error instanceof Error ? error.message : "未知错误"}`,
          "error",
        );
      }
    }
    setBusy(false);
    if (fileInput.current) fileInput.current.value = "";
    if (done) showToast(done === 1 ? "上传完成" : `已上传 ${done} 个文件`);
    await load(path);
  };

  const createFolder = async () => {
    const name = newName.trim().normalize("NFC");
    if (!name) {
      setCreating(false);
      return;
    }
    try {
      const response = await fetch("/api/fs/mkdir", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ path: joinPath(path, name) }),
      });
      const payload = (await response.json()) as ApiResponse<unknown>;
      if (!payload.ok) throw new Error(payload.error ?? "创建失败");
      setCreating(false);
      setNewName("");
      await load(path);
    } catch (error) {
      showToast(error instanceof Error ? error.message : "创建失败", "error");
    }
  };

  const confirmDelete = async () => {
    if (!pendingDelete) return;
    const target = pendingDelete;
    setPendingDelete(null);
    try {
      const response = await fetch("/api/fs/delete", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ path: target.path }),
      });
      const payload = (await response.json()) as ApiResponse<unknown>;
      if (!payload.ok) throw new Error(payload.error ?? "删除失败");
      showToast(`已移入回收站：${target.name}`);
      await load(path);
    } catch (error) {
      showToast(error instanceof Error ? error.message : "删除失败", "error");
    }
  };

  const totalSize = items.reduce((sum, item) => (item.is_dir ? sum : sum + Number(item.size)), 0);
  const fileCount = items.filter((item) => !item.is_dir).length;

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
      <header className={styles.topbar}>
        <div className={styles.topbarInner}>
          <div className={styles.brand}>
            <span className={styles.brandDot} />
            <span className={styles.brandText}>Everything About Me</span>
          </div>

          <div className={styles.topSpacer} />

          <div className={styles.search}>
            <span className={styles.searchIcon}>
              <SearchIcon size={14} />
            </span>
            <input
              ref={searchInput}
              className={styles.searchInput}
              placeholder="搜索"
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

          <button
            className={styles.primaryBtn}
            onClick={() => fileInput.current?.click()}
            disabled={busy}
          >
            <UploadIcon size={14} />
            {busy ? "上传中" : "上传"}
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
        <div className={styles.toolbar}>
          {activeQuery ? (
            <div className={styles.resultNote}>
              <span>
                “{activeQuery}” · {items.length} 个结果
              </span>
              <button className={styles.clearBtn} onClick={() => void load(path)}>
                清除
              </button>
            </div>
          ) : (
            <nav className={styles.crumbs} aria-label="路径">
              {crumbs.map((crumb, index) => {
                const last = index === crumbs.length - 1;
                return (
                  <span key={crumb.path || "root"} style={{ display: "contents" }}>
                    {index > 0 && (
                      <span className={styles.sep}>
                        <ChevronRightIcon size={12} />
                      </span>
                    )}
                    <button
                      className={`${styles.crumb} ${last ? styles.crumbCurrent : ""}`}
                      onClick={() => void load(crumb.path)}
                      aria-current={last ? "page" : undefined}
                    >
                      {crumb.name}
                    </button>
                  </span>
                );
              })}
            </nav>
          )}

          <div className={styles.toolSpacer} />

          <button
            className={styles.ghostBtn}
            onClick={() => {
              setCreating(true);
              setNewName("");
            }}
            title="新建目录"
            aria-label="新建目录"
            disabled={Boolean(activeQuery)}
          >
            <PlusIcon />
          </button>
          <button
            className={styles.ghostBtn}
            onClick={() => void load(path)}
            title="刷新"
            aria-label="刷新"
          >
            <RefreshIcon />
          </button>
        </div>

        <section className={styles.panel} aria-busy={loading}>
          {creating && (
            <div className={styles.newRow}>
              <input
                className={styles.newInput}
                autoFocus
                placeholder="目录名称，回车确认"
                value={newName}
                onChange={(event) => setNewName(event.target.value)}
                onKeyDown={(event) => {
                  if (event.key === "Enter") void createFolder();
                  if (event.key === "Escape") setCreating(false);
                }}
              />
              <button className={styles.textBtn} onClick={() => void createFolder()}>
                创建
              </button>
              <button className={styles.textBtn} onClick={() => setCreating(false)}>
                取消
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
                      onClick={() => void load(item.path)}
                      title={item.name}
                    >
                      {item.name}
                    </button>
                  ) : (
                    <a
                      className={styles.name}
                      href={`/api/fs/download?path=${encodeURIComponent(item.path)}`}
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

                <span className={styles.size}>{item.is_dir ? "" : formatSize(Number(item.size))}</span>
                <span className={styles.time}>{formatTime(item.updated_at)}</span>

                <span className={styles.rowActions}>
                  {!item.is_dir && (
                    <a
                      className={styles.ghostBtn}
                      href={`/api/fs/download?path=${encodeURIComponent(item.path)}`}
                      title="下载"
                      aria-label={`下载 ${item.name}`}
                    >
                      <DownloadIcon size={15} />
                    </a>
                  )}
                  <button
                    className={`${styles.ghostBtn} ${styles.dangerBtn}`}
                    onClick={() => setPendingDelete(item)}
                    title="删除"
                    aria-label={`删除 ${item.name}`}
                  >
                    <TrashIcon size={15} />
                  </button>
                </span>
              </div>
            ))}

          {!loading && items.length === 0 && (
            <div className={styles.empty}>
              <span className={styles.emptyIcon}>
                {activeQuery ? <SearchIcon size={18} /> : <FolderIcon size={18} />}
              </span>
              <span className={styles.emptyTitle}>
                {activeQuery ? "没有找到匹配的文件" : "这里还空着"}
              </span>
              {activeQuery && (
                <span className={styles.emptyText}>
                  换个关键词试试，搜索会匹配文件名和完整路径。
                </span>
              )}
            </div>
          )}
        </section>

        {!loading && items.length > 0 && (fileCount > 0 || totalSize > 0) && (
          <footer className={styles.footer}>
            {fileCount > 0 && <span>{fileCount} 个文件</span>}
            {fileCount > 0 && totalSize > 0 && <span>·</span>}
            {totalSize > 0 && <span>{formatSize(totalSize)}</span>}
          </footer>
        )}
      </main>

      {dragging && (
        <div className={styles.dropOverlay}>
          <div className={styles.dropInner}>
            <UploadIcon size={22} />
            <span>松开即可上传到「{path ? crumbs[crumbs.length - 1]?.name : "全部文件"}」</span>
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
              aria-label="关闭"
            >
              <CloseIcon size={12} />
            </button>
          )}
        </div>
      )}

      {pendingDelete && (
        <div className={styles.backdrop} onClick={() => setPendingDelete(null)}>
          <div className={styles.dialog} onClick={(event) => event.stopPropagation()}>
            <div className={styles.dialogTitle}>删除「{pendingDelete.name}」？</div>
            <p className={styles.dialogText}>
              {pendingDelete.is_dir ? "目录及其中的全部内容" : "文件"}会移入回收站，30 天后才会真正清除。
            </p>
            <div className={styles.dialogActions}>
              <button className={styles.cancelBtn} onClick={() => setPendingDelete(null)}>
                取消
              </button>
              <button className={styles.confirmBtn} onClick={() => void confirmDelete()}>
                删除
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
