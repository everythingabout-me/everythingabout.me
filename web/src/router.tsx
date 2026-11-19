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
import { filesHref, setDriveBase } from "./filesPath";
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
import { WelcomePage } from "./pages/WelcomePage";
import { LoadingFallback } from "./pages/LoadingFallback";
import { RouteError } from "./pages/RouteError";
import type { FileResource } from "./api/client";

async function requireUser(): Promise<AuthUser> {
  const user = await api.me();
  if (!user) throw redirect("/login");
  // Every authenticated entry names the account's address prefix here, so the link builders
  // in filesPath never have to guess whether the tree is at / or at /<handle>.
  setDriveBase(user.username);
  return user;
}

/**
 * Reaching the tree needs an address. An account that has never claimed a handle finishes
 * onboarding at /welcome first: the drive's home IS the handle, so until it exists there is
 * no address to render.
 */
async function requireAddress(): Promise<AuthUser> {
  const user = await requireUser();
  if (!user.username) throw redirect("/welcome");
  return user;
}

/**
 * The browser used to keep the open directory in ?path=. Send those bookmarks (and the old
 * /drive/* URLs) to the handle address instead of losing them.
 */
async function legacyDriveRedirect({ request, params }: LoaderFunctionArgs): Promise<Response> {
  await requireAddress();
  const url = new URL(request.url);
  const path = params["*"] || url.searchParams.get("path") || "";
  return redirect(filesHref(path));
}

/** /files/<path> predates the handle address; one redirect keeps old bookmarks working. */
async function legacyFilesRedirect({ params }: LoaderFunctionArgs): Promise<Response> {
  await requireAddress();
  return redirect(filesHref(params["*"] ?? ""));
}

/**
 * /f/<id> is the stable link to one entry: it names the entry, not its path, so it survives a
 * rename or a move. Opening it reads the entry and sends the browser to its current address —
 * the id form is what you paste, the path form is what you look at.
 */
async function entryLinkLoader({ params }: LoaderFunctionArgs): Promise<Response> {
  await requireAddress();
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
 * The drive route: /<handle>/<path>. A handle that belongs to this account renders the tree;
 * an old handle of this account redirects to the current one; anything else is not an
 * address this account can open.
 */
async function driveLoader({ params }: LoaderFunctionArgs) {
  const user = await requireAddress();
  const handle = (params.username ?? "").toLowerCase();
  if (handle === user.username) return { user };
  const rest = params["*"] ?? "";
  let current: string;
  try {
    current = await api.resolveUsername(handle);
  } catch (error) {
    if (error instanceof ApiError && error.status === 404) {
      throw new Response(t("drive.addressMissing", { handle }), { status: 404 });
    }
    throw error;
  }
  setDriveBase(current);
  return redirect(filesHref(rest));
}

/**
 * The browser-level handler for web+eam://<handle>/<path>. registerProtocolHandler substitutes
 * the clicked URL for %s, so the page exists only to turn it back into a drive address. It is
 * browser-scoped by construction: the OS never calls this, which is why the scheme is web+eam
 * and not eam.
 */
async function protocolOpenLoader({ request }: LoaderFunctionArgs): Promise<Response> {
  const user = await requireAddress();
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
  const handle = target.hostname.toLowerCase();
  const rest = target.pathname.replace(/^\//, "");
  if (!handle) throw new Response(t("drive.addressMissing", { handle: raw }), { status: 404 });
  if (handle === user.username) {
    return redirect(filesHref(safeDecode(rest)));
  }
  let current: string;
  try {
    current = await api.resolveUsername(handle);
  } catch (error) {
    if (error instanceof ApiError && error.status === 404) {
      throw new Response(t("drive.addressMissing", { handle }), { status: 404 });
    }
    throw error;
  }
  setDriveBase(current);
  return redirect(filesHref(safeDecode(rest)));
}

function safeDecode(value: string): string {
  try {
    return decodeURIComponent(value);
  } catch {
    return value;
  }
}

const pageBoundary = {
  errorElement: <RouteError />,
  hydrateFallbackElement: <LoadingFallback />,
};

export const router = createBrowserRouter([
  {
    /*
     * Home hands the browser the account's own address. The tree is not mounted at / and
     * not at /files: an account's content lives under its handle, and those two paths stay
     * free for whatever the site itself needs at the root.
     */
    path: "/",
    loader: async () => {
      await requireAddress();
      return redirect(filesHref("/"));
    },
    ...pageBoundary,
  },
  {
    // The timeline lives beside the file browser rather than on top of it; it has no nav entry.
    path: "/stream",
    element: <StreamPage />,
    loader: async () => ({ user: await requireUser() }),
    ...pageBoundary,
  },
  // /files/... was the pre-handle address of the same tree; it redirects, it is not a mount.
  { path: "/files", loader: legacyFilesRedirect, ...pageBoundary },
  { path: "/files/*", loader: legacyFilesRedirect, ...pageBoundary },
  {
    /*
     * The drive itself, at the account's address. Every folder click changes only the splat
     * on this same route instance, so the default revalidation would spend a blocking
     * /api/auth/me round trip before the listing even starts; the page redirects to /login on
     * a 401 from any file request, so the loader only runs when the tree is entered.
     */
    path: "/:username/*",
    element: <FilesPage />,
    shouldRevalidate: () => false,
    loader: driveLoader,
    ...pageBoundary,
  },
  { path: "/f/:id", loader: entryLinkLoader, ...pageBoundary },
  { path: "/open", loader: protocolOpenLoader, ...pageBoundary },
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
        const summary = await api.account();
        setDriveBase(summary.user.username);
        return { summary };
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
      {
        // Claiming an address is a signed-in step, not part of signing in; it borrows the
        // auth layout because there is no drive to put around it yet.
        path: "/welcome",
        element: <WelcomePage />,
        loader: async () => ({ user: await requireUser() }),
      },
      { path: "*", loader: () => redirect("/") },
    ],
  },
]);
