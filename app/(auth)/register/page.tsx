import { redirect } from "next/navigation";

/** 登录和注册已经合并到 /login */
export default function RegisterPage() {
  redirect("/login");
}
