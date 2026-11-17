import { createBrowserRouter, redirect, type LoaderFunctionArgs } from "react-router";
import {
  ApiError,
  api,
  type AccountSummary,
  type StorageData,
  type StorageConnectionDetail,
  type AuthUser,
  type ConnectData,
} from "./api/client";
import { filesHref } from "./filesPath";
import { AuthLayout } from "./layouts/AuthLayout";
import { FilesPage } from "./pages/FilesPage";
import { LegalPage } from "./pages/LegalPage";
import { SettingsPage } from "./pages/SettingsPage";
import { ConnectSettings } from "./pages/settings/ConnectSettings";
import { GeneralSettings } from "./pages/settings/GeneralSettings";
import { StorageSettings } from "./pages/settings/StorageSettings";
import { StorageProviderSettings } from "./pages/settings/StorageProviderSettings";
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
    /*
     * Home is the file browser, opened at the root of the tree: opening the app is meant to show
     * the files themselves, not a menu that leads to them. Folders below the root keep their own
     * /files/... address so the open folder stays linkable.
     */
    path: "/",
    element: <FilesPage />,
    shouldRevalidate: () => false,
    loader: async () => ({ user: await requireUser() }),
    ...pageBoundary,
  },
  {
    // The timeline lives beside the file browser rather than on top of it; it has no nav entry.
    path: "/stream",
    element: <StreamPage />,
    loader: async () => ({ user: await requireUser() }),
    ...pageBoundary,
  },
  // The root listing is the home page now; old links to the bare browser still work.
  { path: "/files", loader: () => redirect("/"), ...pageBoundary },
  {
    // Matches every logical path below the root: /files/Photos/2024/a.jpg.
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
    /*
     * One settings page with its sections as child routes. The account summary is the page's own
     * data (the header needs the user), so it is loaded here and shared through the outlet; each
     * section loads only what it shows. shouldRevalidate keeps a section switch from repeating
     * the page-level request — the section loaders still run on their own.
     */
    path: "/settings",
    element: <SettingsPage />,
    loader: async (): Promise<{ summary: AccountSummary }> => {
      try {
        return { summary: await api.account() };
      } catch (error) {
        if (error instanceof ApiError && error.status === 401) throw redirect("/login");
        throw error;
      }
    },
    shouldRevalidate: () => false,
    ...pageBoundary,
    children: [
      { index: true, element: <GeneralSettings /> },
      {
        path: "connect",
        element: <ConnectSettings />,
        loader: async (): Promise<{ data: ConnectData }> => {
          try {
            return { data: await api.connect() };
          } catch (error) {
            if (error instanceof ApiError && error.status === 401) throw redirect("/login");
            throw error;
          }
        },
      },
      {
        path: "storage",
        element: <StorageSettings />,
        loader: async (): Promise<{ data: StorageData }> => {
          try {
            return { data: await api.storage() };
          } catch (error) {
            if (error instanceof ApiError && error.status === 401) throw redirect("/login");
            throw error;
          }
        },
      },
      {
        /*
         * One provider's page: its own address, so it can be linked to and the back
         * button works. A provider that no longer exists (removed in another tab) is a
         * list, not an error page.
         */
        path: "storage/:id",
        element: <StorageProviderSettings />,
        loader: async ({ params }): Promise<{ data: StorageConnectionDetail }> => {
          try {
            return { data: await api.storageConnection(params.id ?? "") };
          } catch (error) {
            if (error instanceof ApiError && error.status === 401) throw redirect("/login");
            if (error instanceof ApiError && error.status === 404) throw redirect("/settings/storage");
            throw error;
          }
        },
      },
      { path: "*", loader: () => redirect("/settings") },
    ],
  },
  // The account and connect pages became sections of settings; keep their old URLs working.
  { path: "/account", loader: () => redirect("/settings") },
  { path: "/connect", loader: () => redirect("/settings/connect") },
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
