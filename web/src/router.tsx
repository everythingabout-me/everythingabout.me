import { createBrowserRouter, redirect } from "react-router";
import { ApiError, api, type AccountSummary, type AuthUser } from "./api/client";
import { AuthLayout } from "./layouts/AuthLayout";
import { AccountPage } from "./pages/AccountPage";
import { DrivePage } from "./pages/DrivePage";
import { LoginPage } from "./pages/LoginPage";
import { LoadingFallback } from "./pages/LoadingFallback";
import { RouteError } from "./pages/RouteError";

async function requireUser(): Promise<AuthUser> {
  const user = await api.me();
  if (!user) throw redirect("/login");
  return user;
}

export const router = createBrowserRouter([
  {
    path: "/",
    element: <DrivePage />,
    loader: async () => ({ user: await requireUser() }),
    errorElement: <RouteError />,
    hydrateFallbackElement: <LoadingFallback />,
  },
  {
    element: <AuthLayout />,
    errorElement: <RouteError />,
    hydrateFallbackElement: <LoadingFallback />,
    children: [
      {
        path: "/login",
        element: <LoginPage />,
        // 已经登录的人不该再看到登录页
        loader: async () => {
          if (await api.me()) throw redirect("/");
          return null;
        },
      },
      {
        path: "/account",
        element: <AccountPage />,
        loader: async (): Promise<{ summary: AccountSummary }> => {
          try {
            return { summary: await api.account() };
          } catch (error) {
            if (error instanceof ApiError && error.status === 401) throw redirect("/login");
            throw error;
          }
        },
      },
      // 登录和注册已经合并到 /login
      { path: "/register", loader: () => redirect("/login") },
      { path: "*", loader: () => redirect("/") },
    ],
  },
]);
