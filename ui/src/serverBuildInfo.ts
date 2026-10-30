/** Running build metadata; published fields describe a verified matching release only. */
export interface ServerBuildInfo {
  version: string;
  kind: "stable" | "prerelease" | "development" | "unknown";
  commit?: string;
  built_at?: string;
  build_channel?: string;
  published_version?: string;
  published_at?: string;
}
