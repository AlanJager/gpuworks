import i18n from "i18next";
import LanguageDetector from "i18next-browser-languagedetector";
import { initReactI18next } from "react-i18next";

export const supportedLanguages = ["en", "zh"] as const;
export type SupportedLanguage = (typeof supportedLanguages)[number];

// Keep the glob call syntactically direct so Vite expands it. Node's RTL
// runner has no import.meta.env and therefore skips the branch; tests provide
// their own i18next resources.
const localeModules = typeof import.meta.env !== "undefined"
  ? import.meta.glob("./locales/*/*.json", { eager: true, import: "default" }) as Record<string, Record<string, unknown>>
  : {};

function buildResources() {
  const resources: Record<string, Record<string, Record<string, unknown>>> = {};
  for (const [path, messages] of Object.entries(localeModules)) {
    const match = path.match(/^\.\/locales\/([^/]+)\/([^/]+)\.json$/);
    if (!match) continue;
    const [, language, namespace] = match;
    resources[language] ??= {};
    resources[language][namespace] = messages;
  }
  return resources;
}

const namespaces = [...new Set(Object.keys(localeModules).map((path) => {
  const filename = path.split("/").pop() ?? "";
  return filename.replace(/\.json$/, "");
}).filter(Boolean))];

void i18n
  .use(LanguageDetector)
  .use(initReactI18next)
  .init({
    resources: buildResources(),
    supportedLngs: supportedLanguages,
    fallbackLng: "en",
    defaultNS: "common",
    ns: namespaces,
    load: "languageOnly",
    nonExplicitSupportedLngs: true,
    interpolation: { escapeValue: false },
    detection: {
      order: ["localStorage", "navigator"],
      lookupLocalStorage: "gpuworks.locale",
      caches: ["localStorage"],
    },
    react: { useSuspense: false },
  });

function syncDocumentLanguage(language: string) {
  document.documentElement.lang = language === "zh" ? "zh-CN" : "en";
}

syncDocumentLanguage(i18n.resolvedLanguage ?? i18n.language);
i18n.on("languageChanged", syncDocumentLanguage);

export default i18n;
