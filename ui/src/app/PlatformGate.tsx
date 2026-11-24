import type { ReactNode } from "react";
import { Link, useLocation } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { Notice } from "../ui-kit/Notice";
import { Spinner } from "../ui-kit/Spinner";
import { productMessageText } from "../apiError";
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

/**
 * PlatformGate holds the pages back until the server's limit on the whole
 * server is known (GET /api/v1/console/manifest, platform). Under either
 * limit the pages show: a read-only and a locked server both serve reads, and
 * PlatformNotice says above every page what is refused and where to lift it.
 * The server decides and enforces the limit.
 *
 * A console with extensions waits for the manifest before it shows a page, so
 * a limited server never shows one without its notice; the community console,
 * which nothing limits, never waits.
 */
export default function PlatformGate({ children }: { children: ReactNode }) {
  const { t } = useTranslation();
  const { settled } = useConsolePlatform();
  if (!settled && consoleExtensions().length > 0) return <Spinner label={t("consoleExtension.loading")} />;
  return <>{children}</>;
}

/** PlatformNotice states the limit above every page but the one that lifts it. */
export function PlatformNotice() {
  const { t } = useTranslation();
  const { pathname } = useLocation();
  const { platform } = useConsolePlatform();
  if (!platform || opensPage(platform.link, pathname)) return null;
  const path = consolePath(platform.link);
  const label = platform.action ? productMessageText(t, platform.action) : t("consoleBanners.details");
  const control = "text-label font-medium underline underline-offset-2";
  return (
    <Notice
      role={platform.access === "locked" ? "alert" : "status"}
      tone={platform.access === "locked" ? "danger" : "warning"}
      title={productMessageText(t, platform.message)}
      action={path
        ? <Link className={control} to={path}>{label}</Link>
        : platform.link ? <a className={control} href={platform.link} target="_blank" rel="noopener noreferrer">{label}</a> : undefined}
    />
  );
}
