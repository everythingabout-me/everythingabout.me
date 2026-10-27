import { useLoaderData } from "react-router";
import type { AccountSummary } from "../api/client";
import { AccountPanel } from "../components/AccountPanel";
import { t } from "../i18n";

export function AccountPage() {
  const { summary } = useLoaderData() as { summary: AccountSummary };

  return (
    <>
      <title>{t("meta.accountTitle")}</title>
      <AccountPanel user={summary.user} legacyCount={summary.legacyCount} />
    </>
  );
}
