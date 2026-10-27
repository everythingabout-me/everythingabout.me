import { useState } from "react";
import { api, type SaveLink } from "../api/client";
import { t } from "../i18n";
import { formatRelativeTime } from "../i18n/format";
import styles from "../styles/auth.module.css";

function linkURL(link: SaveLink): string {
  return `${window.location.origin}${link.path}`;
}

export function SaveLinks({ initial }: { initial: SaveLink[] }) {
  const [links, setLinks] = useState(initial.filter((link) => !link.revoked));
  const [busy, setBusy] = useState(false);
  const [revoking, setRevoking] = useState<string | null>(null);
  const [copied, setCopied] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  async function create() {
    setError(null);
    setBusy(true);
    try {
      const result = await api.createSaveLink();
      setLinks((current) => [result.link, ...current]);
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : t("error.saveFailed"));
    } finally {
      setBusy(false);
    }
  }

  async function revoke(id: string) {
    setError(null);
    setRevoking(id);
    try {
      await api.revokeSaveLink(id);
      setLinks((current) => current.filter((link) => link.id !== id));
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : t("error.operationFailed"));
    } finally {
      setRevoking(null);
    }
  }

  async function copy(link: SaveLink) {
    try {
      await navigator.clipboard.writeText(linkURL(link));
      setCopied(link.id);
      window.setTimeout(() => setCopied((current) => (current === link.id ? null : current)), 1600);
    } catch {
      setError(t("account.saveLinks.copyFailed"));
    }
  }

  return (
    <section>
      <div className={styles.saveLinksHead}>
        <div>
          <h2 className={styles.sectionTitle}>{t("account.saveLinks.title")}</h2>
          <p className={styles.sectionHint}>{t("account.saveLinks.hint")}</p>
        </div>
        <button className={styles.ghostBtn} onClick={() => void create()} disabled={busy}>
          {busy ? t("account.saveLinks.creating") : t("account.saveLinks.create")}
        </button>
      </div>

      {error && <p className={styles.error}>{error}</p>}

      {links.length === 0 ? (
        <p className={styles.footnote}>{t("account.saveLinks.empty")}</p>
      ) : (
        <div className={styles.linkList}>
          {links.map((link) => (
            <div className={styles.linkRow} key={link.id}>
              <code className={styles.linkUrl}>{linkURL(link)}</code>
              <p className={styles.linkMeta}>
                {link.lastUsedAt
                  ? t("account.saveLinks.lastUsed", { when: formatRelativeTime(link.lastUsedAt) })
                  : t("account.saveLinks.created", { when: formatRelativeTime(link.createdAt) })}
              </p>
              <div className={styles.linkActions}>
                <button className={styles.ghostBtn} onClick={() => void copy(link)}>
                  {copied === link.id
                    ? t("account.saveLinks.copied")
                    : t("account.saveLinks.copy")}
                </button>
                <button
                  className={styles.removeBtn}
                  onClick={() => void revoke(link.id)}
                  disabled={revoking === link.id}
                >
                  {revoking === link.id
                    ? t("account.saveLinks.revoking")
                    : t("account.saveLinks.revoke")}
                </button>
              </div>
            </div>
          ))}
        </div>
      )}
    </section>
  );
}
