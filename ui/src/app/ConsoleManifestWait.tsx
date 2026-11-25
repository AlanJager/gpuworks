import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Spinner } from "../ui-kit/Spinner";
import { consoleExtensions } from "../extensions";
import { useConsolePlatform } from "../extensions/featureAccess";

/**
 * ConsoleManifestWait holds the pages back until the console manifest has
 * answered, when the console has extensions: the manifest says whether the
 * server is limited as a whole, and a limited server's page never shows
 * without its notice. The community console, which nothing limits, never
 * waits.
 */
export default function ConsoleManifestWait({ children }: { children: ReactNode }) {
  const { t } = useTranslation();
  const { settled } = useConsolePlatform();
  if (!settled && consoleExtensions().length > 0) return <Spinner label={t("consoleExtension.loading")} />;
  return <>{children}</>;
}
