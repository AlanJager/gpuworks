import { getJSON } from "./api";

/**
 * FeatureAccess is how the server says the console may show a feature-keyed
 * entry: full use, read-only, visible but locked, or not at all. The console
 * renders it and never works it out itself.
 */
export type FeatureAccess = "full" | "read_only" | "locked" | "hidden";

/** ConsoleFeature is one feature key's access as GET /api/v1/console/manifest reports it. */
export interface ConsoleFeature {
  key: string;
  access: FeatureAccess;
  /** The server's display hint for the key; the access above already reflects it. */
  presentation?: string;
  /** Where a locked entry's "learn more" link points; absent when the server names none. */
  upsell_url?: string;
}

/** ConsoleNavContribution is a menu entry the server adds (an installed app, for example). */
export interface ConsoleNavContribution {
  id: string;
  group: string;
  label_key: string;
  icon?: string;
  route: string;
  feature?: string;
  source?: string;
  app_instance_id?: string;
}

/** ConsoleBanner is a console-wide notice; ConsoleBanners renders them. */
export interface ConsoleBanner {
  id: string;
  level?: string;
  code?: string;
  params?: Record<string, unknown>;
  message?: string;
  link?: string;
}

/**
 * ConsoleManifest is GET /api/v1/console/manifest: what this server's console
 * shows. A server without extensions answers an empty feature list.
 */
export interface ConsoleManifest {
  edition: string;
  features: ConsoleFeature[];
  nav: ConsoleNavContribution[];
  banners: ConsoleBanner[];
}

export const consoleManifestApi = {
  get: (signal?: AbortSignal) => getJSON<ConsoleManifest>("/api/v1/console/manifest", signal),
};
