import { useEffect, useRef, useState } from "react";
import { useLoaderData } from "react-router";
import {
  api,
  type AuthUser,
  type ConnectData,
  type Connection,
  type ConnectionKind,
  type CreatedConnection,
} from "../api/client";
import { AppHeader } from "../components/AppHeader";
import { PathPicker } from "../components/PathPicker";
import { AppIcon, CheckIcon, CopyIcon, KeyIcon } from "../components/icons";
import { t, type MessageKey } from "../i18n";
import { formatRelativeTime, formatUntil } from "../i18n/format";
import styles from "./ConnectPage.module.css";

type Mode = ConnectionKind;

const DAY = 86_400;

// An app is long-lived: the app names itself, so the page only chooses how much it may reach
// and for how long. A token is short-lived and must expire.
const LIFETIMES: Record<Mode, { value: number; key: MessageKey }[]> = {
  app: [
    { value: 60 * 60, key: "connect.lifetime1h" },
    { value: DAY, key: "connect.lifetime1d" },
    { value: 30 * DAY, key: "connect.lifetime30d" },
    { value: 365 * DAY, key: "connect.lifetime1y" },
    { value: 0, key: "connect.noExpiry" },
  ],
  token: [
    { value: 15 * 60, key: "connect.lifetime15m" },
    { value: 60 * 60, key: "connect.lifetime1h" },
    { value: 8 * 60 * 60, key: "connect.lifetime8h" },
    { value: 24 * 60 * 60, key: "connect.lifetime24h" },
    { value: 7 * DAY, key: "connect.lifetime7d" },
  ],
};

const TOKEN_LIFETIME = 60 * 60;

function isExpired(connection: Connection): boolean {
  return connection.expiresAt !== null && new Date(connection.expiresAt).getTime() <= Date.now();
}

function isActive(connection: Connection): boolean {
  return !connection.revoked && !isExpired(connection);
}

function touched(connection: Connection): number {
  return Math.max(
    new Date(connection.createdAt).getTime(),
    connection.lastUsedAt ? new Date(connection.lastUsedAt).getTime() : 0,
  );
}

function scopeLabel(connection: Connection): string {
  if (!connection.prefix) return t("connect.wholeDrive");
  return `${connection.prefix.replace(/\/+$/, "")}/`;
}

/** An app is minted without a name and names itself later; until then, say so. */
function displayName(connection: Connection): string {
  if (connection.name.trim()) return connection.name;
  return connection.kind === "app" ? t("connect.unnamedApp") : t("connect.unnamedToken");
}

function accessLabel(connection: Connection): string {
  let label: string;
  if (connection.read && connection.write) label = t("connect.accessReadWrite");
  else if (connection.read) label = t("connect.accessRead");
  else if (connection.write) label = t("connect.accessWrite");
  else label = t("connect.accessEvents");
  if (connection.events && (connection.read || connection.write)) {
    label = `${label} + ${t("connect.events")}`;
  }
  return label;
}

function expiryLabel(connection: Connection): string {
  if (!connection.expiresAt) return t("connect.noExpiry");
  return t("connect.expires", { when: formatUntil(connection.expiresAt) });
}

function lastUsedLabel(connection: Connection): string {
  return connection.lastUsedAt
    ? t("connect.lastUsed", { when: formatRelativeTime(connection.lastUsedAt) })
    : t("connect.neverUsed");
}

function lifeLabel(connection: Connection): string {
  if (connection.revoked) {
    return t("connect.revoked", {
      when: formatRelativeTime(connection.revokedAt || connection.createdAt),
    });
  }
  if (isExpired(connection)) {
    return t("connect.expired", { when: formatRelativeTime(connection.expiresAt!) });
  }
  if (connection.kind === "token" || connection.expiresAt) {
    return `${expiryLabel(connection)} · ${lastUsedLabel(connection)}`;
  }
  return lastUsedLabel(connection);
}

/** The one block a human copies to an agent: the credential, what it reaches and how to use it. */
function agentPrompt(created: CreatedConnection, token: Connection): string {
  const scope = token.prefix ? `${token.prefix.replace(/\/+$/, "")}/` : "the whole drive";
  const parts: string[] = [];
  if (token.read) parts.push("read");
  if (token.write) parts.push("write");
  if (token.events) parts.push("events");
  const access = parts.join(" + ") || "no access";
  const listPath = token.prefix ? `/list?path=${encodeURIComponent(token.prefix)}` : "/list";
  const writePath = token.prefix ? `${token.prefix}/note.md` : "Inbox/note.md";

  return `You can access my drive through the Everything About Me API.

API:     ${created.api}
Token:   ${created.secret}
Access:  ${access}
Scope:   ${scope}
Expires: ${token.expiresAt ? new Date(token.expiresAt).toISOString().replace(/\.\d{3}Z$/, "Z") : "never"}

Use it as a Bearer token:
  curl -H "Authorization: Bearer ${created.secret}" \\
       "${created.api}${listPath}"

  curl -X PUT -H "Authorization: Bearer ${created.secret}" \\
       --data-binary @note.md "${created.api}/api/files/${writePath}"

First call GET /me to confirm the token and its scope.`;
}

export function ConnectPage() {
  const { user, data } = useLoaderData() as { user: AuthUser; data: ConnectData };
  const [tokens, setTokens] = useState<Connection[]>(data.tokens ?? []);
  const [tokenForm, setTokenForm] = useState(false);
  const [created, setCreated] = useState<CreatedConnection | null>(null);
  const [adding, setAdding] = useState(false);
  const [revoking, setRevoking] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  // Expiry labels and the active/finished split depend on the clock; refresh them once a minute.
  const [, setTick] = useState(0);
  useEffect(() => {
    const id = window.setInterval(() => setTick((value) => value + 1), 60_000);
    return () => window.clearInterval(id);
  }, []);

  const active = tokens.filter(isActive).sort((left, right) => touched(right) - touched(left));
  const apps = active.filter((token) => token.kind === "app");
  const temporary = active.filter((token) => token.kind === "token");
  const finished = tokens
    .filter((token) => !isActive(token))
    .sort((left, right) => {
      const leftAt = new Date(left.revokedAt || left.expiresAt || left.createdAt).getTime();
      const rightAt = new Date(right.revokedAt || right.expiresAt || right.createdAt).getTime();
      return rightAt - leftAt;
    });

  // "Add app" mints a connection with the defaults and opens the handoff at once: the page
  // configures it while the QR exists, instead of asking for a name before the QR exists.
  async function addApp() {
    if (adding) return;
    setError(null);
    setCreated(null);
    setTokenForm(false);
    setAdding(true);
    try {
      const result = await api.createConnection({
        kind: "app",
        prefix: "",
        read: true,
        write: true,
        events: true,
        expiresIn: 0,
      });
      setTokens((current) => [result.token, ...current]);
      setCreated(result);
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : t("error.operationFailed"));
    } finally {
      setAdding(false);
    }
  }

  function startToken() {
    setError(null);
    setCreated(null);
    setTokenForm(true);
  }

  function onCreated(result: CreatedConnection) {
    setTokens((current) => [result.token, ...current]);
    setCreated(result);
    setTokenForm(false);
  }

  function onUpdated(connection: Connection) {
    setTokens((current) => current.map((item) => (item.id === connection.id ? connection : item)));
  }

  async function revoke(connection: Connection) {
    setError(null);
    setRevoking(connection.id);
    try {
      await api.revokeConnection(connection.id);
      setTokens((current) =>
        current.map((token) =>
          token.id === connection.id ? { ...token, revoked: true, revokedAt: new Date().toISOString() } : token,
        ),
      );
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : t("error.operationFailed"));
    } finally {
      setRevoking(null);
    }
  }

  return (
    <div className={styles.page}>
      <title>{t("meta.connectTitle")}</title>
      <AppHeader user={user} />
      <main className={styles.main}>
        <header className={styles.intro}>
          <div>
            <h1 className={styles.title}>{t("connect.title")}</h1>
            <p className={styles.subtitle}>{t("connect.subtitle")}</p>
          </div>
          <div className={styles.actions}>
            <button className={styles.secondary} type="button" disabled={adding} onClick={() => void addApp()}>
              {adding ? t("connect.creating") : t("connect.addApp")}
            </button>
            <button className={styles.secondary} type="button" onClick={startToken}>
              {t("connect.newToken")}
            </button>
          </div>
        </header>

        {created ? (
          created.token.kind === "app" ? (
            <AppHandoff
              key={created.token.id}
              created={created}
              onUpdated={onUpdated}
              onDone={() => setCreated(null)}
            />
          ) : (
            <TokenHandoff created={created} onDone={() => setCreated(null)} />
          )
        ) : tokenForm ? (
          <TokenPanel onCancel={() => setTokenForm(false)} onCreated={onCreated} />
        ) : null}

        {error && <p className={styles.error}>{error}</p>}

        <ConnectionList
          title={t("connect.appsTitle")}
          items={apps}
          empty={t("connect.emptyApps")}
          revoking={revoking}
          onRevoke={revoke}
        />
        <ConnectionList
          title={t("connect.tokensTitle")}
          items={temporary}
          empty={t("connect.emptyTokens")}
          revoking={revoking}
          onRevoke={revoke}
        />

        {finished.length > 0 && (
          <section className={styles.section}>
            <details className={styles.finished}>
              <summary className={styles.finishedSummary}>
                <span>{t("connect.finished")}</span>
                <span className={styles.count}>{finished.length}</span>
              </summary>
              <div className={styles.list}>
                {finished.map((connection) => (
                  <ConnectionRow key={connection.id} connection={connection} finished />
                ))}
              </div>
            </details>
          </section>
        )}
      </main>
    </div>
  );
}

function TokenPanel({
  onCancel,
  onCreated,
}: {
  onCancel: () => void;
  onCreated: (result: CreatedConnection) => void;
}) {
  const [name, setName] = useState("");
  const [prefix, setPrefix] = useState("");
  const [read, setRead] = useState(true);
  const [write, setWrite] = useState(true);
  const [events, setEvents] = useState(false);
  const [lifetime, setLifetime] = useState(TOKEN_LIFETIME);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const scope = prefix ? `${prefix.replace(/\/+$/, "")}/` : t("connect.wholeDrive");
  const access = read && write
    ? t("connect.accessReadWrite")
    : read
      ? t("connect.accessRead")
      : write
        ? t("connect.accessWrite")
        : t("connect.accessEvents");
  const expiry =
    lifetime === 0
      ? t("connect.noExpiry")
      : t("connect.expires", {
          when: formatUntil(new Date(Date.now() + lifetime * 1000).toISOString()),
        });
  // A token is copied by hand for one agent, so it is named while it is minted.
  const needsName = !name.trim();
  const hasPermission = read || write || events;
  const valid = hasPermission && !needsName;
  const summary = needsName
    ? t("connect.needName")
    : !hasPermission
      ? t("connect.needPermission")
      : `${access}${events && (read || write) ? ` + ${t("connect.events")}` : ""} · ${scope} · ${expiry}`;

  function toggle(next: "read" | "write" | "events") {
    if (next === "read") setRead(!read);
    else if (next === "write") setWrite(!write);
    else setEvents(!events);
  }

  async function submit() {
    if (!valid || busy) return;
    setError(null);
    setBusy(true);
    try {
      const result = await api.createConnection({
        name: name.trim(),
        kind: "token",
        prefix,
        read,
        write,
        events,
        expiresIn: lifetime,
      });
      onCreated(result);
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : t("error.operationFailed"));
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className={styles.panel}>
      <div className={styles.panelHead}>
        <div>
          <h2 className={styles.panelTitle}>{t("connect.formTokenTitle")}</h2>
          <p className={styles.panelHint}>{t("connect.formTokenHint")}</p>
        </div>
        <button className={styles.ghost} type="button" onClick={onCancel}>
          {t("common.cancel")}
        </button>
      </div>

      <div className={styles.form}>
        <label className={styles.field}>
          <span className={styles.label}>{t("connect.name")}</span>
          <input
            className={styles.input}
            value={name}
            maxLength={80}
            autoFocus
            spellCheck={false}
            placeholder={t("connect.nameTokenPlaceholder")}
            onChange={(event) => setName(event.target.value)}
          />
        </label>

        <div className={styles.field}>
          <span className={styles.label}>{t("connect.path")}</span>
          <PathPicker value={prefix} onChange={setPrefix} />
        </div>

        <div className={styles.field}>
          <span className={styles.label}>{t("connect.access")}</span>
          <div className={styles.options}>
            <CheckBox label={t("connect.read")} checked={read} onChange={() => toggle("read")} />
            <CheckBox label={t("connect.write")} checked={write} onChange={() => toggle("write")} />
            <CheckBox label={t("connect.events")} checked={events} onChange={() => toggle("events")} />
          </div>
        </div>

        <div className={styles.field}>
          <span className={styles.label}>{t("connect.lifetime")}</span>
          <div className={styles.options}>
            {LIFETIMES.token.map((option) => (
              <CheckBox
                key={option.value}
                label={t(option.key)}
                checked={lifetime === option.value}
                onChange={() => setLifetime(option.value)}
              />
            ))}
          </div>
        </div>
      </div>

      {error && <p className={styles.error}>{error}</p>}

      <div className={styles.submitRow}>
        <p className={styles.summary}>
          <span className={styles.dot} />
          <span>{summary}</span>
        </p>
        <button className={styles.primary} type="button" disabled={!valid || busy} onClick={() => void submit()}>
          {busy ? t("connect.creating") : t("connect.createToken")}
        </button>
      </div>
    </section>
  );
}

/** One permission or lifetime. Selected is a check in a blue box, not just a tinted pill. */
function CheckBox({ label, checked, onChange }: { label: string; checked: boolean; onChange: () => void }) {
  return (
    <button
      className={`${styles.option} ${checked ? styles.optionOn : ""}`}
      type="button"
      aria-pressed={checked}
      onClick={onChange}
    >
      <span className={styles.check}>{checked && <CheckIcon size={11} />}</span>
      <span>{label}</span>
    </button>
  );
}

function useCopy() {
  const [copied, setCopied] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  async function copy(value: string, key: string) {
    setError(null);
    try {
      await navigator.clipboard.writeText(value);
      setCopied(key);
      window.setTimeout(() => setCopied((current) => (current === key ? null : current)), 1600);
    } catch {
      setError(t("connect.copyFailed"));
    }
  }

  return { copied, error, copy };
}

function HandoffHead({ title, onDone }: { title: string; onDone: () => void }) {
  return (
    <div className={styles.panelHead}>
      <div>
        <h2 className={styles.panelTitle}>{title}</h2>
        <p className={styles.panelHint}>{t("connect.createdHint")}</p>
      </div>
      <button className={styles.secondary} type="button" onClick={onDone}>
        {t("connect.done")}
      </button>
    </div>
  );
}

/**
 * An app is minted first and configured while the handoff is open: the QR on the left is the
 * credential, the prompt is that credential written out for an agent, and the right pane edits
 * the connection in place. The QR carries no policy, so editing it can never make it stale.
 */
function AppHandoff({
  created,
  onUpdated,
  onDone,
}: {
  created: CreatedConnection;
  onUpdated: (connection: Connection) => void;
  onDone: () => void;
}) {
  const [token, setToken] = useState(created.token);
  const [lifetime, setLifetime] = useState(0);
  const [saving, setSaving] = useState(false);
  const [configError, setConfigError] = useState<string | null>(null);
  const savingRef = useRef(false);
  const { copied, error: copyError, copy } = useCopy();
  const prompt = agentPrompt(created, token);

  async function configure(next: {
    prefix?: string;
    read?: boolean;
    write?: boolean;
    events?: boolean;
    lifetime?: number;
  }) {
    if (savingRef.current) return;
    const prefix = next.prefix ?? token.prefix;
    const read = next.read ?? token.read;
    const write = next.write ?? token.write;
    const events = next.events ?? token.events;
    const life = next.lifetime ?? lifetime;
    if (!read && !write && !events) {
      setConfigError(t("connect.needPermission"));
      return;
    }
    if (
      prefix === token.prefix &&
      read === token.read &&
      write === token.write &&
      events === token.events &&
      life === lifetime
    ) {
      return;
    }

    // The controls answer immediately; the request only confirms the computed expiry.
    const previous = token;
    const previousLifetime = lifetime;
    const expiresAt = life === 0 ? null : new Date(Date.now() + life * 1000).toISOString();
    setToken({ ...token, prefix, read, write, events, expiresAt });
    setLifetime(life);
    savingRef.current = true;
    setSaving(true);
    setConfigError(null);
    try {
      const saved = await api.updateConnection(token.id, { prefix, read, write, events, expiresIn: life });
      setToken(saved.token);
      onUpdated(saved.token);
    } catch (caught) {
      setToken(previous);
      setLifetime(previousLifetime);
      setConfigError(caught instanceof Error ? caught.message : t("error.operationFailed"));
    } finally {
      savingRef.current = false;
      setSaving(false);
    }
  }

  return (
    <section className={styles.handoff}>
      <HandoffHead title={t("connect.createdAppTitle")} onDone={onDone} />

      <div className={styles.handoffGrid}>
        <div className={styles.handoffLeft}>
          <div className={styles.qrWrap}>
            <div className={styles.qrCard}>
              <img className={styles.qr} src={created.qr} width={220} height={220} alt={t("connect.createdAppTitle")} />
            </div>
            <p className={styles.qrHint}>{t("connect.scanHint", { api: created.api })}</p>
          </div>

          <CodeBlock
            label={t("connect.promptLabel")}
            text={prompt}
            copied={copied === "prompt"}
            onCopy={() => void copy(prompt, "prompt")}
          />

          <details className={styles.jsonDetails}>
            <summary className={styles.jsonSummary}>{t("connect.jsonDetails")}</summary>
            <CodeBlock
              label="JSON"
              text={created.payload}
              copied={copied === "json"}
              onCopy={() => void copy(created.payload, "json")}
            />
          </details>
          {copyError && <p className={styles.error}>{copyError}</p>}
        </div>

        <aside className={styles.configPane}>
          <h3 className={styles.configTitle}>{t("connect.configTitle")}</h3>
          <div className={styles.configFields}>
            <div className={styles.field}>
              <span className={styles.label}>{t("connect.path")}</span>
              <PathPicker value={token.prefix} onChange={(prefix) => void configure({ prefix })} />
            </div>

            <div className={styles.field}>
              <span className={styles.label}>{t("connect.access")}</span>
              <div className={styles.options}>
                <CheckBox
                  label={t("connect.read")}
                  checked={token.read}
                  onChange={() => void configure({ read: !token.read })}
                />
                <CheckBox
                  label={t("connect.write")}
                  checked={token.write}
                  onChange={() => void configure({ write: !token.write })}
                />
                <CheckBox
                  label={t("connect.events")}
                  checked={token.events}
                  onChange={() => void configure({ events: !token.events })}
                />
              </div>
            </div>

            <div className={styles.field}>
              <span className={styles.label}>{t("connect.lifetime")}</span>
              <div className={styles.options}>
                {LIFETIMES.app.map((option) => (
                  <CheckBox
                    key={option.value}
                    label={t(option.key)}
                    checked={lifetime === option.value}
                    onChange={() => void configure({ lifetime: option.value })}
                  />
                ))}
              </div>
            </div>
          </div>
          {configError && <p className={styles.error}>{configError}</p>}
          <p className={styles.configHint}>{saving ? t("connect.saving") : t("connect.configHint")}</p>
        </aside>
      </div>
    </section>
  );
}

function TokenHandoff({ created, onDone }: { created: CreatedConnection; onDone: () => void }) {
  const { copied, error, copy } = useCopy();
  const token = created.token;
  const text = agentPrompt(created, token);

  return (
    <section className={styles.handoff}>
      <HandoffHead title={t("connect.createdTokenTitle")} onDone={onDone} />
      <CodeBlock
        label={t("connect.setupLabel")}
        text={text}
        copied={copied === "setup"}
        onCopy={() => void copy(text, "setup")}
      />
      <p className={styles.scopeLine}>
        {accessLabel(token)} · {scopeLabel(token)} · {expiryLabel(token)}
      </p>
      {error && <p className={styles.error}>{error}</p>}
    </section>
  );
}

function CodeBlock({
  label,
  text,
  copied,
  onCopy,
}: {
  label: string;
  text: string;
  copied: boolean;
  onCopy: () => void;
}) {
  return (
    <div className={styles.codeBlock}>
      <div className={styles.codeHead}>
        <span className={styles.codeLabel}>{label}</span>
        <button className={styles.copyBtn} type="button" onClick={onCopy}>
          {copied ? <CheckIcon size={13} /> : <CopyIcon size={13} />}
          {copied ? t("connect.copied") : t("connect.copy")}
        </button>
      </div>
      <pre className={styles.codePre}>{text}</pre>
    </div>
  );
}

function ConnectionList({
  title,
  items,
  empty,
  revoking,
  onRevoke,
}: {
  title: string;
  items: Connection[];
  empty: string;
  revoking: string | null;
  onRevoke: (connection: Connection) => Promise<void>;
}) {
  return (
    <section className={styles.section}>
      <div className={styles.sectionHead}>
        <h2 className={styles.sectionTitle}>{title}</h2>
        <span className={styles.count}>{items.length}</span>
      </div>
      <div className={styles.list}>
        {items.length === 0 ? (
          <p className={styles.empty}>{empty}</p>
        ) : (
          items.map((connection) => (
            <ConnectionRow
              key={connection.id}
              connection={connection}
              revoking={revoking === connection.id}
              onRevoke={() => onRevoke(connection)}
            />
          ))
        )}
      </div>
    </section>
  );
}

function ConnectionRow({
  connection,
  finished = false,
  revoking = false,
  onRevoke,
}: {
  connection: Connection;
  finished?: boolean;
  revoking?: boolean;
  onRevoke?: () => void;
}) {
  return (
    <div className={`${styles.row} ${finished ? styles.rowFinished : ""}`}>
      <span className={styles.glyph}>
        {connection.kind === "app" ? <AppIcon size={15} /> : <KeyIcon size={15} />}
      </span>
      <div className={styles.rowMain}>
        <div className={styles.rowName} title={displayName(connection)}>
          {displayName(connection)}
        </div>
        <div className={styles.rowMeta}>
          {finished && (
            <>
              <span className={styles.kind}>{t(connection.kind === "app" ? "connect.appKind" : "connect.tokenKind")}</span>
              <span className={styles.sep}>·</span>
            </>
          )}
          <span>{accessLabel(connection)}</span>
          <span className={styles.sep}>·</span>
          <span>{scopeLabel(connection)}</span>
        </div>
        <div className={styles.rowLife}>{lifeLabel(connection)}</div>
      </div>
      {onRevoke && (
        <RevokeButton
          busy={revoking}
          label={t("connect.revoke")}
          confirmLabel={t("connect.confirmRevoke")}
          busyLabel={t("connect.revoking")}
          onConfirm={onRevoke}
        />
      )}
    </div>
  );
}

function RevokeButton({
  busy,
  label,
  confirmLabel,
  busyLabel,
  onConfirm,
}: {
  busy: boolean;
  label: string;
  confirmLabel: string;
  busyLabel: string;
  onConfirm: () => void;
}) {
  const [confirming, setConfirming] = useState(false);

  useEffect(() => {
    if (!confirming) return;
    const id = window.setTimeout(() => setConfirming(false), 4000);
    return () => window.clearTimeout(id);
  }, [confirming]);

  return (
    <button
      className={`${styles.revoke} ${confirming ? styles.revokeConfirm : ""}`}
      type="button"
      disabled={busy}
      onClick={() => {
        if (confirming) {
          setConfirming(false);
          onConfirm();
        } else {
          setConfirming(true);
        }
      }}
    >
      {busy ? busyLabel : confirming ? confirmLabel : label}
    </button>
  );
}
