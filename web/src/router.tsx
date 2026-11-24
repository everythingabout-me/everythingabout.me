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
import { t } from "./i18n";
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
import type { FileResource } from "./api/client";

async function requireUser(): Promise<AuthUser> {
  const user = await api.me();
  if (!user) throw redirect("/login");
  return user;
}

/**
 * The browser used to keep the open directory in ?path=. Send those bookmarks (and the old
 * /drive/* URLs) to the file address instead of losing them.
 */
async function legacyDriveRedirect({ request, params }: LoaderFunctionArgs): Promise<Response> {
  await requireUser();
  const url = new URL(request.url);
  const path = params["*"] || url.searchParams.get("path") || "";
  return redirect(filesHref(path));
}

/**
 * /f/<id> is the stable link to one entry: it names the entry, not its path, so it survives a
 * rename or a move. Opening it reads the entry and sends the browser to its current address —
 * the id form is what you paste, the path form is what you look at.
 */
async function entryLinkLoader({ params }: LoaderFunctionArgs): Promise<Response> {
  await requireUser();
  let entry: FileResource;
  try {
    entry = await api.metadata({ id: params.id ?? "" });
  } catch (error) {
    if (error instanceof ApiError && error.status === 404) {
      throw new Response(t("drive.entryMissing"), { status: 404 });
    }
    throw error;
  }
  return redirect(filesHref(entry.path));
}

/**
 * The browser-level handler for web+eam:///files/<path>. registerProtocolHandler substitutes
 * the clicked URL for %s, so the page exists only to turn it back into a drive address. It is
 * browser-scoped by construction: the OS never calls this, which is why the scheme is web+eam
 * and not eam.
 *
 * The custom scheme carries the address in its path, not its host: there is no account name in
 * a drive address any more, and a hostname that held a CJK path would have to be punycoded.
 */
async function protocolOpenLoader({ request }: LoaderFunctionArgs): Promise<Response> {
  await requireUser();
  const raw = new URL(request.url).searchParams.get("url") ?? "";
  let target: URL;
  try {
    target = new URL(raw);
  } catch {
    throw new Response(t("drive.protocolUrlInvalid"), { status: 400 });
  }
  if (target.protocol !== "web+eam:") {
    throw new Response(t("drive.protocolUrlInvalid"), { status: 400 });
  }
  const destination = `${target.pathname}${target.search}`;
  if (!destination.startsWith("/") || destination.startsWith("//")) {
    throw new Response(t("drive.protocolUrlInvalid"), { status: 400 });
  }
  return redirect(destination);
}

const pageBoundary = {
  errorElement: <RouteError />,
  hydrateFallbackElement: <LoadingFallback />,
};

export const router = createBrowserRouter([
  {
    // Home is the drive's own address. The tree is mounted under /files, so this redirect is
    // all the site root owes anyone; it does not check the session first, because the drive
    // route does that and a brand click must not spend an /api/auth/me round trip being told
    // what it already knows.
    path: "/",
    loader: async () => redirect(filesHref("/")),
    ...pageBoundary,
  },
  {
    // The timeline lives beside the file browser rather than on top of it; it has no nav entry.
    path: "/stream",
    element: <StreamPage />,
    loader: async () => ({ user: await requireUser() }),
    ...pageBoundary,
  },
  // /drive/* kept the open directory in ?path= before the tree had an address; one redirect
  // keeps those bookmarks working.
  { path: "/drive", loader: legacyDriveRedirect, ...pageBoundary },
  { path: "/drive/*", loader: legacyDriveRedirect, ...pageBoundary },
  {
    /*
     * The drive itself, from its root (`/files`, the splat matches empty) down every folder: one
     * route instance, so clicking into a folder changes only the splat and the page keeps its
     * state — an upload queue must survive browsing — instead of remounting. That also means the
     * default revalidation would spend a blocking /api/auth/me round trip before the listing even
     * starts, so it is off; the page redirects to /login on a 401 from any file request, and the
     * loader only runs when the tree is entered.
     */
    path: "/files/*",
    element: <FilesPage />,
    shouldRevalidate: () => false,
    loader: async () => ({ user: await requireUser() }),
    ...pageBoundary,
  },
  { path: "/f/:id", loader: entryLinkLoader, ...pageBoundary },
  { path: "/open", loader: protocolOpenLoader, ...pageBoundary },
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
