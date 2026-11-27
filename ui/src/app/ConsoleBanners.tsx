import { Link, useLocation } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { Notice } from "../ui-kit/Notice";
import { productMessageText } from "../apiError";
import { useConsoleBanners, useConsolePlatform } from "../extensions/featureAccess";
import { useBannerAck } from "../extensions/bannerAck";
import { Button } from "../components/ui/button";
import PlatformNotice, { opensPage, platformNoticeShown } from "./PlatformNotice";

const tones: Record<string, "info" | "warning" | "danger"> = { info: "info", warning: "warning", danger: "danger" };

/**
 * ConsoleBanners shows the server's banners (GET /api/v1/console/manifest)
 * above every page. Which banners exist, for whom and how severe is the
 * server's decision; this only renders them. The manifest read is the one the
 * extension entries share. While the manifest loads, when
 * it has none, or when it cannot be read, nothing is shown: banners are never
 * what a page needs to work. A banner whose link opens the page already shown
 * is left out there: that page states the matter itself. A limit on the whole
 * server (platform) is stated first, by the same rule. A banner the server
 * gives an ack_key offers an acknowledge control; once the viewer uses it, a
 * banner with that key is no longer shown to them (bannerAck.ts).
 */
export default function ConsoleBanners() {
  const { t } = useTranslation();
  const { pathname } = useLocation();
  const ack = useBannerAck();
  const banners = useConsoleBanners().filter((banner) => !opensPage(banner.link, pathname) && !ack.acknowledged(banner.ack_key));
  const { platform } = useConsolePlatform();
  if (banners.length === 0 && !platformNoticeShown(platform, pathname)) return null;
  return (
    <section aria-label={t("consoleBanners.region")} className="mb-4 grid gap-2">
      <PlatformNotice />
      {banners.map((banner) => (
        <Notice
          key={banner.id}
          role={banner.severity === "danger" ? "alert" : "status"}
          tone={tones[banner.severity] ?? "info"}
          title={productMessageText(t, banner.message)}
          action={banner.link || banner.ack_key ? <>
            {banner.link && <BannerLink to={banner.link} label={t("consoleBanners.details")} />}
            {banner.ack_key && <Button variant="ghost" size="sm" onClick={() => ack.acknowledge(banner.ack_key!)}>{t("consoleBanners:acknowledge")}</Button>}
          </> : undefined}
        />
      ))}
    </section>
  );
}

// BannerLink opens a console path in place and any other address in a new tab.
function BannerLink({ to, label }: { to: string; label: string }) {
  const className = "text-label font-medium underline underline-offset-2";
  if (to.startsWith("/") && !to.startsWith("//")) return <Link className={className} to={to}>{label}</Link>;
  return <a className={className} href={to} target="_blank" rel="noopener noreferrer">{label}</a>;
}
