import { useLoaderData } from "react-router";
import type { AccountSummary } from "../api/client";
import { AccountPanel } from "../components/AccountPanel";

export function AccountPage() {
  const { summary } = useLoaderData() as { summary: AccountSummary };

  return (
    <>
      <title>账户 · Everything About Me</title>
      <AccountPanel user={summary.user} legacyCount={summary.legacyCount} />
    </>
  );
}
