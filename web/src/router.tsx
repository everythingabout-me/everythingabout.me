import { createBrowserRouter, redirect, type LoaderFunctionArgs } from "react-router";
import { ApiError, api, type AccountSummary, type AuthUser } from "./api/client";
import { filesHref } from "./filesPath";
import { AuthLayout } from "./layouts/AuthLayout";
import { AccountPage } from "./pages/AccountPage";
import { FilesPage } from "./pages/FilesPage";
import { StreamPage } from "./pages/StreamPage";
import { LoginPage } from "./pages/LoginPage";
import { LoadingFallback } from "./pages/LoadingFallback";
import { RouteError } from "./pages/RouteError";

async function requireUser(): Promise<AuthUser> {
  const user = await api.me();
  if (!user) throw redirect("/login");
  return user;
}

/**
 * The browser used to keep the open directory in ?path=. Send those bookmarks (and the old
 * /drive/* URLs) to the path-based /files route instead of losing them.
 */
function legacyDriveRedirect({ request, params }: LoaderFunctionArgs): Response {
  const url = new URL(request.url);
  const path = params["*"] || url.searchParams.get("path") || "";
  return redirect(filesHref(path));
}

const pageBoundary = {
  errorElement: <RouteError />,
  hydrateFallbackElement: <LoadingFallback />,
};

export const router = createBrowserRouter([
  {
    path: "/",
    element: <StreamPage />,
    loader: async () => ({ user: await requireUser() }),
    ...pageBoundary,
  },
  {
    // Matches /files and every logical path below it: /files/Photos/2024/a.jpg.
    path: "/files/*",
    element: <FilesPage />,
    loader: async () => ({ user: await requireUser() }),
    ...pageBoundary,
  },
  {
    path: "/drive",
    loader: legacyDriveRedirect,
    ...pageBoundary,
  },
  {
    path: "/drive/*",
    loader: legacyDriveRedirect,
    ...pageBoundary,
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
    ...pageBoundary,
  },
  {
    element: <AuthLayout />,
    ...pageBoundary,
    children: [
      {
        path: "/login",
        element: <LoginPage />,
        // Someone already signed in should not see the login page again
        loader: async () => {
          if (await api.me()) throw redirect("/");
          return null;
        },
      },
      // Sign-in and sign-up are merged into /login
      { path: "/register", loader: () => redirect("/login") },
      { path: "*", loader: () => redirect("/") },
    ],
  },
]);
