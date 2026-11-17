import { useTranslation } from "react-i18next";
import { Result } from "../ui-kit/Result";
import type { LockedSlotProps } from "./types";

/** LockedFeature is the default page of a locked entry, with the server's "learn more" link when it names one. */
export function LockedFeature({ link }: LockedSlotProps) {
  const { t } = useTranslation("common");
  return (
    <Result
      title={t("consoleExtension.locked.title")}
      description={t("consoleExtension.locked.description")}
      actions={link ? <a href={link} target="_blank" rel="noopener noreferrer">{t("consoleExtension.locked.learnMore")}</a> : undefined}
    />
  );
}
