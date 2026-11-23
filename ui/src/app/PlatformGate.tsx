import type { ReactNode } from "react";
import { Link, Navigate, useLocation } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { Notice } from "../ui-kit/Notice";
import { Spinner } from "../ui-kit/Spinner";
import { productMessageText } from "../apiError";
import type { ConsolePlatform } from "../consoleManifest";
import { consoleExtensions } from "../extensions";
import { useConsolePlatform } from "../extensions/featureAccess";

/** consolePath is link when it is a path of this console, else undefined. */
export function consolePath(link: string | undefined): string | undefined {
  return link && link.startsWith("/") && !link.startsWith("//") ? link : undefined;
}

/** opensPage reports whether a console link (query and fragment aside) is the page at pathname. */
export function opensPage(link: string | undefined, pathname: string): boolean {
  const path = consolePath(link);
  return path !== undefined && trimSlash(path.replace(/[?#].*$/, "")) === trimSlash(pathname);
}

const trimSlash = (path: string) => (path.length > 1 ? path.replace(/\/+$/, "") : path);

/** lockedOut reports whether the server's limit leaves the page at pathname unreachable. */
export function lockedOut(platform: ConsolePlatform | undefined, pathname: string): boolean {
  return platform?.access === "locked" && !opensPage(platform.link, pathname);
}

/**
 * PlatformGate shows the pages only as far as the server's limit on the whole
 * server allows (GET /api/v1/console/manifest, platform). Locked, every page
 * but the one that lifts the limit goes there; when the server names no such
 * page, the limit's own message stands in for the page. Read-only changes
 * nothing here: the pages show and the server refuses changes. The server
 * decides and enforces the limit; this only keeps the console from offering
 * what will be refused.
 *
 * A console with extensions waits for the manifest before it shows a page, so
 * a locked server never flashes one; the community console, which nothing
 * limits, never waits.
 */
export default function PlatformGate({ children }: { children: ReactNode }) {
  const { t } = useTranslation();
  const { pathname } = useLocation();
  const { platform, settled } = useConsolePlatform();
  if (!settled && consoleExtensions().length > 0) return <Spinner label={t("consoleExtension.loading")} />;
  if (!lockedOut(platform, pathname)) return <>{children}</>;
  const path = consolePath(platform?.link);
  if (path) return <Navigate replace to={path} />;
  return (
    <Notice
      role="alert"
      tone="danger"
      title={productMessageText(t, platform!.message)}
      action={platform!.link ? <a className="text-label font-medium underline underline-offset-2" href={platform!.link} target="_blank" rel="noopener noreferrer">{platform!.action ? productMessageText(t, platform!.action) : t("consoleBanners.details")}</a> : undefined}
    />
  );
}

/** PlatformNotice states the limit above every page but the one that lifts it. */
export function PlatformNotice() {
  const { t } = useTranslation();
  const { pathname } = useLocation();
  const { platform } = useConsolePlatform();
  if (!platform || opensPage(platform.link, pathname)) return null;
  const path = consolePath(platform.link);
  return (
    <Notice
      role={platform.access === "locked" ? "alert" : "status"}
      tone={platform.access === "locked" ? "danger" : "warning"}
      title={productMessageText(t, platform.message)}
      action={path ? <Link className="text-label font-medium underline underline-offset-2" to={path}>{platform.action ? productMessageText(t, platform.action) : t("consoleBanners.details")}</Link> : undefined}
    />
  );
}
