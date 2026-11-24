import { Link, useLocation } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { Notice } from "../ui-kit/Notice";
import { productMessageText } from "../apiError";
import type { ConsolePlatform } from "../consoleManifest";
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

/** platformNoticeShown reports whether the limit is stated above the page at pathname: on every page but the one that lifts it. */
export function platformNoticeShown(platform: ConsolePlatform | undefined, pathname: string): platform is ConsolePlatform {
  return platform !== undefined && !opensPage(platform.link, pathname);
}

/**
 * PlatformNotice states the limit on the whole server (GET
 * /api/v1/console/manifest, platform) above every page but the one that
 * lifts it: what is refused, and a control that opens that page when the
 * server names one for this user. Read-only and locked servers both serve
 * reads, so the pages show under it. The server decides and enforces the
 * limit.
 */
export default function PlatformNotice() {
  const { t } = useTranslation();
  const { pathname } = useLocation();
  const { platform } = useConsolePlatform();
  if (!platformNoticeShown(platform, pathname)) return null;
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
