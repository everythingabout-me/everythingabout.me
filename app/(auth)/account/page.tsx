import { redirect } from "next/navigation";
import { getSessionUser } from "@/lib/auth";
import { query } from "@/lib/db";
import { AccountPanel } from "./account-panel";

export const dynamic = "force-dynamic";

export const metadata = {
  title: "账户 · Everything About Me",
};

export default async function AccountPage() {
  const user = await getSessionUser();
  if (!user) redirect("/login");

  const { rows } = await query<{ n: string }>(
    "select count(*) as n from nodes where owner_id is null and deleted_at is null",
  );

  return <AccountPanel user={user} legacyCount={Number(rows[0]?.n ?? 0)} />;
}
