import { useEffect, useSyncExternalStore } from "react";
import { useTranslation } from "react-i18next";
import { api, type Branding } from "./api";

/** DEFAULT_PRODUCT_NAME is the product name when none is configured. */
export const DEFAULT_PRODUCT_NAME = "GPUWorks";

/** BRANDING_LOGO_MAX_BYTES mirrors the server's logo limit (decoded bytes). */
export const BRANDING_LOGO_MAX_BYTES = 128 << 10;

const EMPTY: Branding = { product_name: "", console_title: "", login_description: "", logo: "" };

// One copy for the whole page: the sign-in page, the shell and the settings
// card read it, and a save replaces it so every place updates at once.
let current: Branding = EMPTY;
let loading: Promise<void> | undefined;
const listeners = new Set<() => void>();

function emit() {
  for (const listener of listeners) listener();
}

function subscribe(listener: () => void) {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
}

/** setBranding replaces the shared branding, e.g. with a save's response. */
export function setBranding(next: Branding) {
  current = { ...EMPTY, ...next };
  emit();
}

/**
 * loadBranding fetches the branding once per page; a failure keeps the
 * defaults, since branding must never stand between a user and the console.
 */
export function loadBranding(): Promise<void> {
  loading ??= api.branding().then(setBranding, () => {});
  return loading;
}

/** resetBrandingForTest forgets the loaded branding. */
export function resetBrandingForTest() {
  current = EMPTY;
  loading = undefined;
  emit();
}

export interface ResolvedBranding {
  productName: string;
  consoleTitle: string;
  loginDescription: string;
  /** The custom logo, or "" for the built-in mark. */
  logo: string;
}

/** useBranding returns the branding with defaults filled in for this locale. */
export function useBranding(): ResolvedBranding {
  const { t } = useTranslation("common");
  const branding = useSyncExternalStore(subscribe, () => current);
  useEffect(() => { void loadBranding(); }, []);
  const productName = branding.product_name || DEFAULT_PRODUCT_NAME;
  return {
    productName,
    consoleTitle: branding.console_title || t("branding.defaultConsoleTitle", { product: productName }),
    loginDescription: branding.login_description || t("branding.defaultLoginDescription"),
    logo: branding.logo,
  };
}

/** BrandMark is the logo: the custom image, or the built-in "G" mark. */
export function BrandMark({ logo }: { logo: string }) {
  if (logo) return <img className="app-brand-mark app-brand-logo" src={logo} alt="" aria-hidden="true" />;
  return <span className="app-brand-mark" aria-hidden="true">G</span>;
}

/**
 * useDocumentBranding keeps the tab title and favicon on the configured
 * product name and logo.
 */
export function useDocumentBranding({ productName, logo }: ResolvedBranding) {
  useEffect(() => {
    document.title = productName;
  }, [productName]);
  useEffect(() => {
    const link = document.querySelector<HTMLLinkElement>("link[rel~='icon']");
    if (!link) return;
    const original = { href: link.getAttribute("href"), type: link.getAttribute("type") };
    if (!logo) return;
    link.href = logo;
    link.type = logo.slice("data:".length, logo.indexOf(";"));
    return () => {
      if (original.href !== null) link.setAttribute("href", original.href);
      if (original.type !== null) link.setAttribute("type", original.type);
    };
  }, [logo]);
}
