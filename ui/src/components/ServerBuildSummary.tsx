import { Badge, type BadgeProps } from "./ui/badge";
import { useTranslation } from "react-i18next";
import { formatTime } from "../i18n/datetime";
import type { ServerBuildInfo } from "../serverBuildInfo";

// A candidate update must never lend its release label to another running build.
export default function ServerBuildSummary({ version, build }: { version: string; build?: ServerBuildInfo }) {
  const { t } = useTranslation("common");
  const info = build?.version === version ? build : undefined;
  const kind = info && ["stable", "prerelease", "development"].includes(info.kind) ? info.kind : "unknown";
  const tones: Record<string, Pick<BadgeProps, "status" | "color">> = { stable: { status: "success" }, prerelease: { status: "warning" }, development: { color: "orange" }, unknown: { color: "gray" } };
  return <div className="flex w-full flex-col gap-1">
    <div className="flex flex-wrap items-center gap-2"><Badge {...tones[kind]}>{t(`buildInfo.kind.${kind}`)}</Badge><span className="font-semibold">{version}</span></div>
    <span>{t(`buildInfo.description.${kind}`)}</span>
    {info?.built_at && <span className="text-fg-muted">{t("buildInfo.builtAt", { time: formatTime(info.built_at) })}</span>}
    {info?.published_version ? <span className="text-fg-muted">{t("buildInfo.matched", { version: info.published_version })}{info.published_at && ` · ${t("buildInfo.publishedAt", { time: formatTime(info.published_at) })}`}</span>
      : <span className="text-fg-muted">{t("buildInfo.unmatched")}</span>}
    {(info?.commit || info?.build_channel) && <details>
      <summary>{t("buildInfo.details")}</summary>
      <div className="flex flex-col gap-0.5">
        {info.commit && <code className="w-fit rounded-sm bg-emphasis px-1 font-mono text-label">{t("buildInfo.commit", { commit: info.commit })}</code>}
        {info.build_channel && <span>{t("buildInfo.channel", { channel: info.build_channel })}</span>}
      </div>
    </details>}
  </div>;
}
