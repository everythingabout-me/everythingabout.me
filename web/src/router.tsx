import { createBrowserRouter, redirect, type LoaderFunctionArgs } from "react-router";
import {
  ApiError,
  api,
  type AccountSummary,
  type StorageData,
  type AuthUser,
  type ConnectData,
} from "./api/client";
import { filesHref } from "./filesPath";
import { AuthLayout } from "./layouts/AuthLayout";
import { AccountPage } from "./pages/AccountPage";
import { ConnectPage } from "./pages/ConnectPage";
import { FilesPage } from "./pages/FilesPage";
import { LegalPage } from "./pages/LegalPage";
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
    // Every folder click is a new splat value on this same route instance, so the default
    // revalidation would spend a blocking `/api/auth/me` round trip before the listing even
    // starts. The page redirects to /login on a 401 from any file request, so the loader only
    // needs to run when the page is entered.
    shouldRevalidate: () => false,
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
    path: "/connect",
    element: <ConnectPage />,
    loader: async (): Promise<{ user: AuthUser; data: ConnectData }> => {
      try {
        return { user: await requireUser(), data: await api.connect() };
      } catch (error) {
        if (error instanceof ApiError && error.status === 401) throw redirect("/login");
        throw error;
      }
    },
    ...pageBoundary,
  },
  {
    path: "/account",
    element: <AccountPage />,
    loader: async (): Promise<{ summary: AccountSummary; storage: StorageData }> => {
      try {
        return { summary: await api.account(), storage: await api.storage() };
      } catch (error) {
        if (error instanceof ApiError && error.status === 401) throw redirect("/login");
        throw error;
      }
    },
    ...pageBoundary,
  },
  // Public legal pages: Google OAuth branding, app stores and payment providers all need a
  // policy and terms URL that anyone can open without an account.
  {
    path: "/privacy",
    element: <LegalPage documentKey="privacy" />,
    ...pageBoundary,
  },
  {
    path: "/terms",
    element: <LegalPage documentKey="terms" />,
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
