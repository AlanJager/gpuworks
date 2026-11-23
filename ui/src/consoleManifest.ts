// Wire types of GET /api/v1/console/manifest (dto/console.go): what the
// console shell shows beyond its own pages. The server decides every value;
// the console only renders it.
import { getJSON } from "./api";
import type { ProductMessage } from "./apiError";

export type FeatureAccess = "full" | "read_only" | "locked" | "hidden";

export interface ConsoleFeature {
  key: string;
  access: FeatureAccess;
  presentation: "promote" | "hidden";
  link?: string;
  message?: ProductMessage;
  // A null max is unlimited.
  limits: { dim: string; max: number | null }[];
}

export interface NavContribution {
  id: string;
  group: string;
  label_key: string;
  icon?: string;
  route: string;
  feature?: string;
}

export interface ConsoleBanner {
  id: string;
  severity: "info" | "warning" | "danger";
  message: ProductMessage;
  link?: string;
}

/**
 * ConsolePlatform is a limit on the whole server: read_only shows every page
 * and the server refuses changes; locked leaves only the page at link.
 */
export interface ConsolePlatform {
  access: "read_only" | "locked";
  link?: string;
  // What the control that opens link says; absent leaves the console's own wording.
  action?: ProductMessage;
  message: ProductMessage;
}

export interface ConsoleManifest {
  edition: string;
  features: ConsoleFeature[];
  nav: NavContribution[];
  banners: ConsoleBanner[];
  // Absent while nothing is limited.
  platform?: ConsolePlatform;
}

// An object rather than bare functions, so tests can replace the request.
export const consoleManifestApi = {
  manifest: (signal?: AbortSignal) => getJSON<ConsoleManifest>("/api/v1/console/manifest", signal),
};
