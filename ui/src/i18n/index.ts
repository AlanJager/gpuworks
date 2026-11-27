import i18n from "i18next";
import LanguageDetector from "i18next-browser-languagedetector";
import { initReactI18next } from "react-i18next";

export const supportedLanguages = ["en", "zh"] as const;
export type SupportedLanguage = (typeof supportedLanguages)[number];

type LocaleModule = { default: Record<string, Record<string, unknown>> };

// One chunk per language, so a tab downloads the language it shows and
// English, the fallback: an extension's bundle may carry English only, and a
// few labels are formatted in English on purpose (getFixedT("en")). Node's
// RTL runner has no import.meta.env and loads none: tests provide their own
// resources.
const localeChunks: Partial<Record<SupportedLanguage, () => Promise<LocaleModule>>> = typeof import.meta.env !== "undefined"
  ? { en: () => import("./locales-en"), zh: () => import("./locales-zh") }
  : {};

const loaded = new Map<SupportedLanguage, Promise<void>>();

/**
 * loadConsoleLanguage adds a language's own messages once; switch to a
 * language after it. They win over a console extension's bundle for the same
 * key, as when they were built in.
 */
export function loadConsoleLanguage(language: SupportedLanguage): Promise<void> {
  const chunk = localeChunks[language];
  if (!chunk) return Promise.resolve();
  let pending = loaded.get(language);
  if (!pending) {
    pending = chunk().then(({ default: modules }) => {
      for (const [path, messages] of Object.entries(modules)) {
        const namespace = (path.split("/").pop() ?? "").replace(/\.json$/, "");
        i18n.addResourceBundle(language, namespace, messages, true, true);
      }
    });
    pending.catch(() => loaded.delete(language));
    loaded.set(language, pending);
  }
  return pending;
}

function supported(language: string | undefined): SupportedLanguage {
  const code = (language ?? "").split("-")[0];
  return (supportedLanguages as readonly string[]).includes(code) ? code as SupportedLanguage : "en";
}


void i18n
  .use(LanguageDetector)
  .use(initReactI18next)
  .init({
    resources: {},
    supportedLngs: supportedLanguages,
    fallbackLng: "en",
    defaultNS: "common",
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

/**
 * consoleLanguageReady settles once the detected language's messages and the
 * English fallback are in, so the first render shows text rather than keys. A
 * chunk that failed to load leaves the keys showing rather than holding the
 * console back.
 */
export const consoleLanguageReady: Promise<void> = Object.keys(localeChunks).length === 0
  ? Promise.resolve()
  : Promise.allSettled([loadConsoleLanguage("en"), loadConsoleLanguage(supported(i18n.language))])
    .then(() => i18n.changeLanguage(supported(i18n.language))).then(() => {}, () => {});

i18n.on("languageChanged", syncDocumentLanguage);

export default i18n;
