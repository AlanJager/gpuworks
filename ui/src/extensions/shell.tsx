import { useMemo, type ComponentType, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Spinner } from "../ui-kit/Spinner";
import { navGroups, type NavGroup } from "../navigation";
import { useFeatureResolver, type Availability } from "./featureAccess";
import { consoleExtensions, extensionHeaderItems, mergeNavGroups, needsManifest } from "./index";
import { LockedFeature } from "./LockedFeature";
import type { ExtensionRoute, LockedSlotProps, SlotProps } from "./types";

// The console frame's side of the overlay contract: it shows each extension
// entry the way the server's manifest says (full, read-only, locked, hidden)
// and decides nothing about availability itself.

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

/** ExtensionRoutePage is an extension page under the frame; a hidden page answers as notFound. */
export function ExtensionRoutePage({ route, notFound }: { route: ExtensionRoute; notFound: ReactNode }) {
  const availabilityOf = useFeatureResolver(Boolean(route.feature));
  return <FeatureSlot availability={availabilityOf(route.feature)} component={route.component} locked={route.locked} fallback={notFound} />;
}

/**
 * ExtensionHeaderItems renders the extensions' top bar controls that the
 * server reports full or read-only. The top bar has no room for a locked
 * control, so a locked one is left out like a hidden one.
 */
export function ExtensionHeaderItems() {
  const items = extensionHeaderItems();
  const availabilityOf = useFeatureResolver(items.some((item) => Boolean(item.feature)));
  return (
    <>
      {items.map((item) => {
        const { access } = availabilityOf(item.feature);
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
  return useMemo(() => mergeNavGroups(navGroups, extensions, availabilityOf), [extensions, availabilityOf]);
}

/** useExtensionAvailability resolves feature keys against the manifest, loading it only when an extension needs it. */
export function useExtensionAvailability(): (feature?: string) => Availability {
  return useFeatureResolver(needsManifest(consoleExtensions()));
}
