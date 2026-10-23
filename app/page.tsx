import { redirect } from "next/navigation";
import { getSessionUser } from "@/lib/auth";
import DriveClient from "./drive-client";

export const dynamic = "force-dynamic";

export const metadata = {
  title: "Everything About Me",
};

export default async function Page() {
  const user = await getSessionUser();
  if (!user) redirect("/login");
  return <DriveClient user={user} />;
}
