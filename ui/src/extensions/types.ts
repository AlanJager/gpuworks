import type { ComponentType, ReactNode } from "react";

/**
 * SlotProps reach every component an extension contributes. readOnly is true
 * when the server reports the entry's feature read-only: the component shows
 * its data and offers no write. An entry without a feature is always writable.
 */
export interface SlotProps {
  readOnly: boolean;
}

/** LockedSlotProps reach an entry's own locked page; link is the server's "learn more" address. */
export interface LockedSlotProps {
  link?: string;
}

/**
 * Feature keys are opaque to the console: the server's manifest says how an
 * entry carrying one is shown (full, read-only, locked or hidden), and an entry
 * whose key the manifest does not list is hidden.
 */
interface FeatureKeyed {
  feature?: string;
}

/** ExtensionRoute is a page under the console frame, matched before the not-found page. */
export interface ExtensionRoute extends FeatureKeyed {
  /** A react-router path, e.g. "/audit" or "/audit/:id". */
  path: string;
  component: ComponentType<SlotProps>;
  /** Replaces the default locked page when the feature is locked. */
  locked?: ComponentType<LockedSlotProps>;
}

/** ExtensionNavGroup is a new menu group, placed before administration. */
export interface ExtensionNavGroup {
  id: string;
  labelKey: string;
}

/**
 * ExtensionNavItem is a menu entry. group names a NavGroup id from
 * navigation.tsx or an ExtensionNavGroup; the entry goes to the end of it.
 * labelKey may name its namespace ("audit:nav.title").
 */
export interface ExtensionNavItem extends FeatureKeyed {
  group: string;
  key: string;
  icon: ReactNode;
  labelKey: string;
  also?: string[];
}

/** ExtensionSettingsTab is a tab on the settings page, after the built-in ones. */
export interface ExtensionSettingsTab extends FeatureKeyed {
  /** The URL fragment that opens the tab (/settings#key). */
  key: string;
  labelKey: string;
  /** Card fragments that open this tab, like the built-in tabs' anchors. */
  anchors?: string[];
  component: ComponentType<SlotProps>;
  locked?: ComponentType<LockedSlotProps>;
}

/** ExtensionHeaderItem is a control in the top bar, before the assistant button. */
export interface ExtensionHeaderItem extends FeatureKeyed {
  key: string;
  component: ComponentType<SlotProps>;
}

/** ExtensionLocales holds translations by language, then namespace. */
export type ExtensionLocales = Record<string, Record<string, Record<string, unknown>>>;

/**
 * ConsoleExtension is what a console overlay module (ui/src/extensions/<id>/index.ts)
 * exports as its default. Every list is optional.
 */
export interface ConsoleExtension {
  id: string;
  routes?: ExtensionRoute[];
  navGroups?: ExtensionNavGroup[];
  navItems?: ExtensionNavItem[];
  settingsTabs?: ExtensionSettingsTab[];
  headerItems?: ExtensionHeaderItem[];
  locales?: ExtensionLocales;
}
