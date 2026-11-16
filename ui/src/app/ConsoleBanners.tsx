import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { Notice } from "../ui-kit/Notice";
import { productMessageText } from "../apiError";
import { consoleManifestApi, type ConsoleBanner } from "../consoleManifest";

const tones: Record<string, "info" | "warning" | "danger"> = { info: "info", warning: "warning", danger: "danger" };

/**
 * ConsoleBanners shows the server's banners (GET /api/v1/console/manifest)
 * above every page. Which banners exist, for whom and how severe is the
 * server's decision; this only renders them. While the manifest loads, when
 * it has none, or when it cannot be read, nothing is shown: banners are never
 * what a page needs to work.
 */
export default function ConsoleBanners() {
  const { t } = useTranslation();
  const [banners, setBanners] = useState<ConsoleBanner[]>([]);

  useEffect(() => {
    const controller = new AbortController();
    consoleManifestApi.manifest(controller.signal)
      .then((manifest) => { if (!controller.signal.aborted) setBanners(manifest.banners ?? []); })
      .catch(() => { /* No banners; the page itself is unaffected. */ });
    return () => controller.abort();
  }, []);

  if (banners.length === 0) return null;
  return (
    <section aria-label={t("consoleBanners.region")} className="mb-4 grid gap-2">
      {banners.map((banner) => (
        <Notice
          key={banner.id}
          role={banner.severity === "danger" ? "alert" : "status"}
          tone={tones[banner.severity] ?? "info"}
          title={productMessageText(t, banner.message)}
          action={banner.link ? <BannerLink to={banner.link} label={t("consoleBanners.details")} /> : undefined}
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
