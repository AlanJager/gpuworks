import { useMemo, type ComponentType, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Spinner } from "../ui-kit/Spinner";
import { navGroups, type NavGroup } from "../navigation";
import { useSessionUser } from "../components/sessionUser";
import { useFeatureResolver, type Availability } from "./featureAccess";
import { accountMenuItems, consoleExtensions, entryAvailability, extensionHeaderItems, mergeNavGroups, needsManifest, type AccountMenuEntry } from "./index";
import { LockedFeature } from "./LockedFeature";
import type { ExtensionRoute, LockedSlotProps, SlotProps } from "./types";

// The console frame's side of the overlay contract: it shows each extension
// entry the way the server's manifest says (full, read-only, locked, hidden)
// and decides nothing about availability itself. Admin-only entries follow the
// signed-in user's role from the session read (display only: the server
// refuses their routes to other users).

/**
 * useViewerIsAdmin reports whether the signed-in user is an administrator,
 * from the user the session check already read. False when that user could
 * not be read, so admin-only entries never show by mistake.
 */
export function useViewerIsAdmin(): boolean {
  return useSessionUser()?.role === "admin";
}

/**
 * FeatureSlot renders one entry for its availability: the component (with
 * readOnly when the server says so), the locked page, a wait while the
 * manifest loads, or the fallback when the entry is hidden.
 */
export function FeatureSlot({ availability, component: Component, locked: Locked = LockedFeature, fallback = null }: {
  availability: Availability;
  component: ComponentType<SlotProps>;
  locked?: ComponentType<LockedSlotProps>;
  fallback?: ReactNode;
}) {
  const { t } = useTranslation("common");
  switch (availability.access) {
    case "full":
      return <Component readOnly={false} />;
    case "read_only":
      return <Component readOnly />;
    case "locked":
      return <Locked link={availability.link} />;
    case "pending":
      return <Spinner label={t("consoleExtension.loading")} />;
    default:
      return <>{fallback}</>;
  }
}

/** ExtensionRoutePage is an extension page under the frame; a hidden page, or an admin-only one for anyone else, answers as notFound. */
export function ExtensionRoutePage({ route, notFound }: { route: ExtensionRoute; notFound: ReactNode }) {
  const availabilityOf = useFeatureResolver(Boolean(route.feature));
  const isAdmin = useViewerIsAdmin();
  return <FeatureSlot availability={entryAvailability(route, availabilityOf, isAdmin)} component={route.component} locked={route.locked} fallback={notFound} />;
}

/**
 * ExtensionHeaderItems renders the extensions' top bar controls that the
 * server reports full or read-only. The top bar has no room for a locked
 * control, so a locked one is left out like a hidden one.
 */
export function ExtensionHeaderItems() {
  const items = extensionHeaderItems();
  const availabilityOf = useFeatureResolver(items.some((item) => Boolean(item.feature)));
  const isAdmin = useViewerIsAdmin();
  return (
    <>
      {items.map((item) => {
        const { access } = entryAvailability(item, availabilityOf, isAdmin);
        if (access !== "full" && access !== "read_only") return null;
        const Component = item.component;
        return <Component key={item.key} readOnly={access === "read_only"} />;
      })}
    </>
  );
}

/** useConsoleNavGroups is the side menu: the built-in groups merged with the extensions' entries. */
export function useConsoleNavGroups(): readonly NavGroup[] {
  const extensions = consoleExtensions();
  const availabilityOf = useFeatureResolver(needsManifest(extensions));
  const isAdmin = useViewerIsAdmin();
  return useMemo(() => mergeNavGroups(navGroups, extensions, availabilityOf, isAdmin), [extensions, availabilityOf, isAdmin]);
}

/** useAccountMenuItems is the extensions' entries of the account menu, as the viewer may see them. */
export function useAccountMenuItems(): readonly AccountMenuEntry[] {
  const extensions = consoleExtensions();
  const availabilityOf = useFeatureResolver(needsManifest(extensions));
  const isAdmin = useViewerIsAdmin();
  return useMemo(() => accountMenuItems(extensions, availabilityOf, isAdmin), [extensions, availabilityOf, isAdmin]);
}

/** useExtensionAvailability resolves feature keys against the manifest, loading it only when an extension needs it. */
export function useExtensionAvailability(): (feature?: string) => Availability {
  return useFeatureResolver(needsManifest(consoleExtensions()));
}
