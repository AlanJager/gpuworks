import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { DropdownContent, DropdownMenu, DropdownTrigger } from "@/components/ui/dropdown";
import { MenuItem } from "@/components/ui/menu-item";
import { loadConsoleLanguage, type SupportedLanguage } from "../i18n";
import { message } from "../ui-kit/feedback";
import { ConsoleIcon } from "../ui-kit/consoleIcons";

const GlobeIcon = ConsoleIcon.language;

const languages = [
  { key: "en", label: "English", compact: "EN" },
  { key: "zh", label: String.fromCodePoint(0x7b80, 0x4f53, 0x4e2d, 0x6587), compact: String.fromCodePoint(0x4e2d, 0x6587) },
] as const;

/**
 * LanguageSwitcher switches only once the language's messages are in. A chunk
 * that cannot be fetched (often replaced by an upgrade while the tab stayed
 * open) keeps the current language and says so. loadLanguage is the console's
 * own unless a test passes one.
 */
export default function LanguageSwitcher({ light = false, loadLanguage = loadConsoleLanguage }: { light?: boolean; loadLanguage?: (language: SupportedLanguage) => Promise<void> }) {
  const { t, i18n } = useTranslation();
  const current = i18n.resolvedLanguage === "zh" ? "zh" : "en";
  const index = languages.findIndex((language) => language.key === current);
  return (
    <DropdownMenu>
      <DropdownTrigger
        render={(
          <Button variant={light ? "ghost" : "secondary"} size="md" leadingIcon={GlobeIcon} aria-label={t("language.switch")} title={t("language.switch")}>
            {languages[index].compact}
          </Button>
        )}
      />
      <DropdownContent align="end" checkedIndex={index}>
        {languages.map((language, i) => (
          <MenuItem
            key={language.key}
            index={i}
            label={language.label}
            checked={language.key === current}
            onSelect={() => void loadLanguage(language.key).then(
              () => i18n.changeLanguage(language.key),
              () => { message.error(t("language:loadFailed")); },
            )}
          />
        ))}
      </DropdownContent>
    </DropdownMenu>
  );
}
