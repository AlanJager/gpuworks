import type { NavGroup, NavItem } from "../navigation";
import type { Availability } from "./featureAccess";
import type { ConsoleExtension, ExtensionHeaderItem, ExtensionRoute, ExtensionSettingsTab } from "./types";

// Console overlays: a separately built console copies its modules into
// ui/src/extensions/<id>/ (git-ignored here) and each index.ts default-exports
// a ConsoleExtension. The community tree has none, so this list is empty and
// the menu, settings tabs, routes and top bar are exactly the built-in ones.
// Keep the glob call syntactically direct so Vite expands it; Node's test
// runner has no import.meta.env and skips it (tests register their own
// extensions with setConsoleExtensionsForTest).
const overlayModules = typeof import.meta.env !== "undefined"
  ? import.meta.glob("./*/index.{ts,tsx}", { eager: true, import: "default" }) as Record<string, ConsoleExtension>
  : {};

const builtIn = Object.values(overlayModules);
let registered: readonly ConsoleExtension[] = checkedExtensions(builtIn);

/**
 * checkedExtensions orders extensions by id and refuses two contributions to
 * the same id, route, menu entry, settings tab or top bar control: a clash is
 * an overlay build mistake, so it fails loudly instead of one entry
 * shadowing the other.
 */
function checkedExtensions(extensions: readonly ConsoleExtension[]): readonly ConsoleExtension[] {
  const seen = new Map<string, string>();
  const claim = (what: string, owner: string) => {
    const previous = seen.get(what);
    if (previous !== undefined) throw new Error(`console extension ${owner}: ${what} is already contributed by ${previous}`);
    seen.set(what, owner);
  };
  for (const extension of extensions) {
    if (!extension || typeof extension.id !== "string" || !/^[a-z][a-z0-9_]{1,31}$/.test(extension.id)) {
      throw new Error(`console extension id ${JSON.stringify(extension?.id)} must match ^[a-z][a-z0-9_]{1,31}$`);
    }
    claim(`extension ${extension.id}`, extension.id);
    for (const route of extension.routes ?? []) claim(`route ${route.path}`, extension.id);
    for (const group of extension.navGroups ?? []) claim(`menu group ${group.id}`, extension.id);
    for (const item of extension.navItems ?? []) claim(`menu entry ${item.key}`, extension.id);
    for (const tab of extension.settingsTabs ?? []) claim(`settings tab ${tab.key}`, extension.id);
    for (const item of extension.headerItems ?? []) claim(`top bar control ${item.key}`, extension.id);
  }
  return [...extensions].sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
}

/** consoleExtensions lists the registered console overlays, by id. */
export function consoleExtensions(): readonly ConsoleExtension[] {
  return registered;
}

/**
 * setConsoleExtensionsForTest replaces the registered overlays; undefined
 * restores the ones the build found. Call it before rendering.
 */
export function setConsoleExtensionsForTest(extensions: readonly ConsoleExtension[] | undefined) {
  registered = checkedExtensions(extensions ?? builtIn);
}

/** needsManifest reports whether any registered entry carries a feature key, i.e. whether the console must ask the server. */
export function needsManifest(extensions: readonly ConsoleExtension[] = registered): boolean {
  return extensions.some((extension) => [
    ...(extension.routes ?? []), ...(extension.navItems ?? []), ...(extension.settingsTabs ?? []), ...(extension.headerItems ?? []),
  ].some((entry) => Boolean(entry.feature)));
}

/** ACCOUNT_GROUP is the reserved menu group whose entries go to the account menu, not the side menu. */
export const ACCOUNT_GROUP = "account";

const absent = (availability: Availability) => availability.access === "hidden" || availability.access === "pending";
const FULL: Availability = { access: "full" };
const HIDDEN: Availability = { access: "hidden" };

/**
 * entryAvailability answers how one extension entry is shown: hidden from a
 * viewer who is not an administrator when the entry is admin-only, otherwise
 * what the manifest says of its feature key (full without one, so the
 * manifest is asked only for keyed entries).
 */
export function entryAvailability(
  entry: { feature?: string; admin?: true },
  availabilityOf: (feature?: string) => Availability,
  isAdmin: boolean,
): Availability {
  if (entry.admin && !isAdmin) return HIDDEN;
  return entry.feature ? availabilityOf(entry.feature) : FULL;
}

/**
 * mergeNavGroups adds the extensions' menu groups (before administration) and
 * entries (at the end of their group) to the built-in side menu; entries of
 * the account group are not side menu entries and are skipped. An entry the
 * server hides, or whose availability is not known yet, or an admin-only one
 * when isAdmin is false, is left out; a locked one stays, marked locked; a
 * group left empty is dropped. With no extension side menu entries it returns
 * base itself.
 */
export function mergeNavGroups(
  base: readonly NavGroup[],
  extensions: readonly ConsoleExtension[],
  availabilityOf: (feature?: string) => Availability,
  isAdmin = false,
): readonly NavGroup[] {
  const sideEntries = (extension: ConsoleExtension) => (extension.navItems ?? []).filter((entry) => entry.group !== ACCOUNT_GROUP);
  if (!extensions.some((extension) => sideEntries(extension).length || extension.navGroups?.length)) return base;
  const groups = base.map((group) => ({ ...group, items: [...group.items] }));
  const builtInKeys = new Set(base.flatMap((group) => group.items.map((item) => item.key)));
  const added = new Set<string>();
  const adminAt = groups.findIndex((group) => group.id === "admin");
  const newGroups: NavGroup[] = [];
  for (const extension of extensions) {
    for (const group of extension.navGroups ?? []) {
      if (group.id === ACCOUNT_GROUP || groups.some((existing) => existing.id === group.id)) throw new Error(`console extension ${extension.id}: menu group ${group.id} is built in`);
      newGroups.push({ id: group.id, labelKey: group.labelKey, items: [] });
      added.add(group.id);
    }
  }
  groups.splice(adminAt < 0 ? groups.length : adminAt, 0, ...newGroups);
  for (const extension of extensions) {
    for (const entry of sideEntries(extension)) {
      if (builtInKeys.has(entry.key)) throw new Error(`console extension ${extension.id}: menu entry ${entry.key} is built in`);
      const group = groups.find((candidate) => candidate.id === entry.group);
      if (!group) throw new Error(`console extension ${extension.id}: menu entry ${entry.key} names unknown group ${entry.group}`);
      if (entry.icon === undefined) throw new Error(`console extension ${extension.id}: menu entry ${entry.key} needs an icon in the side menu`);
      const availability = entryAvailability(entry, availabilityOf, isAdmin);
      if (absent(availability)) continue;
      const item: NavItem = { key: entry.key, icon: entry.icon, labelKey: entry.labelKey };
      if (entry.also) item.also = entry.also;
      if (availability.access === "locked") item.locked = true;
      group.items.push(item);
    }
  }
  return groups.filter((group) => !added.has(group.id) || group.items.length > 0);
}

/** AccountMenuEntry is an extension entry of the account menu: the path it opens and its label. */
export interface AccountMenuEntry {
  key: string;
  labelKey: string;
}

/**
 * accountMenuItems lists the extensions' account menu entries (group
 * "account"), in extension order. The menu has no room for a locked entry, so
 * only full and read-only ones are listed, like top bar controls; hidden,
 * not-yet-answered and, for a viewer who is not an administrator, admin-only
 * entries are left out.
 */
export function accountMenuItems(
  extensions: readonly ConsoleExtension[],
  availabilityOf: (feature?: string) => Availability,
  isAdmin = false,
): AccountMenuEntry[] {
  const entries: AccountMenuEntry[] = [];
  for (const extension of extensions) {
    for (const entry of extension.navItems ?? []) {
      if (entry.group !== ACCOUNT_GROUP) continue;
      const { access } = entryAvailability(entry, availabilityOf, isAdmin);
      if (access === "full" || access === "read_only") entries.push({ key: entry.key, labelKey: entry.labelKey });
    }
  }
  return entries;
}

/** ResolvedSettingsTab is an extension's settings tab with its availability. */
export interface ResolvedSettingsTab {
  key: string;
  anchors: string[];
  labelKey: string;
  extension: ExtensionSettingsTab;
  availability: Availability;
}

/**
 * mergeSettingsTabs appends the extensions' settings tabs after the built-in
 * ones, leaving out the ones the server hides or has not answered for yet, and
 * admin-only ones when isAdmin is false. With no extension tabs it returns
 * base itself.
 */
export function mergeSettingsTabs<T extends { key: string; anchors: string[] }>(
  base: readonly T[],
  extensions: readonly ConsoleExtension[],
  availabilityOf: (feature?: string) => Availability,
  isAdmin = false,
): readonly (T | ResolvedSettingsTab)[] {
  if (!extensions.some((extension) => extension.settingsTabs?.length)) return base;
  const builtIn = new Set(base.flatMap((tab) => [tab.key, ...tab.anchors]));
  const merged: (T | ResolvedSettingsTab)[] = [...base];
  for (const extension of extensions) {
    for (const tab of extension.settingsTabs ?? []) {
      for (const name of [tab.key, ...(tab.anchors ?? [])]) {
        if (builtIn.has(name)) throw new Error(`console extension ${extension.id}: settings fragment ${name} is built in`);
      }
      const availability = entryAvailability(tab, availabilityOf, isAdmin);
      if (absent(availability)) continue;
      merged.push({ key: tab.key, anchors: tab.anchors ?? [], labelKey: tab.labelKey, extension: tab, availability });
    }
  }
  return merged;
}

/** isExtensionTab tells an extension's tab from a built-in one. */
export function isExtensionTab<T extends object>(tab: T | ResolvedSettingsTab): tab is ResolvedSettingsTab {
  return "extension" in tab;
}

/** extensionRoutes lists every extension page, in extension order. */
export function extensionRoutes(extensions: readonly ConsoleExtension[] = registered): readonly ExtensionRoute[] {
  return extensions.flatMap((extension) => extension.routes ?? []);
}

/** extensionHeaderItems lists every extension top bar control, in extension order. */
export function extensionHeaderItems(extensions: readonly ConsoleExtension[] = registered): readonly ExtensionHeaderItem[] {
  return extensions.flatMap((extension) => extension.headerItems ?? []);
}

interface ResourceStore {
  addResourceBundle(language: string, namespace: string, resources: Record<string, unknown>, deep?: boolean, overwrite?: boolean): unknown;
}

/**
 * registerExtensionLocales adds the extensions' translations to an i18next
 * instance. Bundles merge deep without overwriting, so an extension can add
 * keys to a built-in namespace (its product codes under common's apiError) but
 * never replace built-in text.
 */
export function registerExtensionLocales(i18n: ResourceStore, extensions: readonly ConsoleExtension[] = registered) {
  for (const extension of extensions) {
    for (const [language, namespaces] of Object.entries(extension.locales ?? {})) {
      for (const [namespace, resources] of Object.entries(namespaces)) {
        i18n.addResourceBundle(language, namespace, resources, true, false);
      }
    }
  }
}
