import { useState } from "react";
import { t } from "../i18n";
import settingsStyles from "../pages/settings/settings.module.css";

/** The scheme a browser can register. `eam:` is not offered: the HTML rule is web+<name>. */
const SCHEME = "web+eam";

/**
 * Registering this drive as the handler for web+eam:// links.
 *
 * The browser, not the OS: registerProtocolHandler only lets one origin claim a scheme
 * inside that browser profile, and the spec only allows a custom scheme when it starts
 * with `web+`. So the address here is web+eam://<handle>/<path>, and an eam:// link from
 * Finder or a terminal still needs a native helper — which the repository ships.
 *
 * There is no API to ask whether this origin already registered, so the card reports what
 * the click did (asked the browser) rather than claiming a state it cannot read.
 */
export function BrowserProtocol({ handle }: { handle: string | null }) {
  const supported =
    typeof navigator !== "undefined" && typeof navigator.registerProtocolHandler === "function";
  const [asked, setAsked] = useState(false);
  const [error, setError] = useState<string | null>(null);

  function register() {
    setError(null);
    if (!supported) return;
    try {
      navigator.registerProtocolHandler(SCHEME, `${window.location.origin}/open?url=%s`);
      setAsked(true);
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : t("error.unknown"));
    }
  }

  return (
    <section className={settingsStyles.card}>
      <div className={settingsStyles.cardHead}>
        <div className={settingsStyles.cardHeadText}>
          <h2 className={settingsStyles.cardTitle}>{t("connect.linksTitle")}</h2>
          <p className={settingsStyles.cardLead}>{t("connect.linksLead")}</p>
        </div>
      </div>

      <p className={settingsStyles.cardLead}>
        {t("connect.schemeExample", { handle: handle ?? "you" })}
      </p>

      {supported ? (
        <>
          <button
            className={settingsStyles.cardAction}
            type="button"
            onClick={register}
            disabled={asked}
          >
            {t("connect.registerScheme")}
          </button>
          {asked && <p className={settingsStyles.cardLead}>{t("connect.schemeAsked")}</p>}
        </>
      ) : (
        <p className={settingsStyles.cardLead}>{t("connect.schemeUnsupported")}</p>
      )}
      {error && <p className={settingsStyles.cardLead} role="alert">{error}</p>}
    </section>
  );
}
