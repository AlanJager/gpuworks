package store

// SettingKey is the identity of a platform setting row. GetSetting/SetSetting
// take it rather than a plain string so a new key has to be declared here
// instead of appearing as a literal at the call site.
//
// Keys reaching the store from an HTTP path parameter (the generic
// /settings/{key} route) are converted explicitly at that boundary; everything
// else names one of the constants below.
type SettingKey string

const (
	// SettingNetwork is the global network/acceleration preset (URL rewrites
	// plus HTTP proxy) applied to every node without an override.
	SettingNetwork SettingKey = "network"
	// SettingNetworkNodePrefix prefixes a per-node network override. Build the
	// full key with NodeNetworkSettingKey rather than concatenating by hand.
	SettingNetworkNodePrefix SettingKey = "network:"

	// SettingImageRegistryRewrites and SettingImageRegistryMirrors held the
	// registry rewrite configuration pull source rules replaced. Only
	// migration 153 reads them, to turn them into rules; no setting is
	// written under either key any more.
	SettingImageRegistryRewrites SettingKey = "image_registry_rewrites"
	SettingImageRegistryMirrors  SettingKey = "image_registry_mirrors"
	// SettingImageCommunityCatalog holds the operator-supplied community image
	// catalog document.
	SettingImageCommunityCatalog SettingKey = "image_community_catalog"

	// SettingOfficialCatalogs records the official catalogs' last imports as one
	// JSON value (store.OfficialCatalogState).
	SettingOfficialCatalogs SettingKey = "official_catalogs"

	// SettingUsageReportLastDay is the UTC day (YYYY-MM-DD) of the last
	// usage report delivered, or of the day before the program started on
	// this installation, so a report always covers a whole observed day.
	SettingUsageReportLastDay SettingKey = "usage_report_last_day"

	// SettingTerminalPolicy holds the web terminal policy (kill switch,
	// concurrency limits, idle timeout) as one JSON value.
	SettingTerminalPolicy SettingKey = "terminal"

	// SettingDeploymentPolicy holds deployment scheduling policy as one JSON
	// value: how long a deployment may sit unscheduled before it is reported as
	// failed, and how long its timeline is kept.
	//
	// Two independent failure modes need bounding. A pending timeout is a time
	// bound; event retention needs both a time and a size bound, because a burst
	// of failures outgrows a size budget long before it is old, and an old
	// backlog outlives its usefulness long before it is large.
	SettingDeploymentPolicy SettingKey = "deployment"

	// SettingOrphanCleanup is a JSON boolean gating the agent sweep that removes
	// gpuworks-named containers missing from the node's desired state. Disabled
	// by default; the value travels to agents inside every full state sync.
	SettingOrphanCleanup SettingKey = "orphan_cleanup"

	// SettingMCPEnabled is whether /api/v1/mcp serves (JSON true/false);
	// absent means off.
	SettingMCPEnabled SettingKey = "mcp.enabled"

	// SettingControlPlane holds this installation's identity on shared
	// storages as one JSON value (store.ControlPlaneIdentity): a generated id
	// and a display name other control planes see in its heartbeat.
	SettingControlPlane SettingKey = "control_plane"

	// SettingStoragePolicy holds the platform-wide storage settings as one
	// JSON value (store.StoragePolicy).
	SettingStoragePolicy SettingKey = "storage_policy"

	// SettingSecurityPolicy holds the platform-wide security switches as one
	// JSON value (store.SecurityPolicy).
	SettingSecurityPolicy SettingKey = "security"

	// SettingBranding holds the console's product name, titles and logo as one
	// JSON value (store.Branding). Only its own route writes it; the generic
	// settings routes neither list nor accept it.
	SettingBranding SettingKey = "branding"

	// SettingTelemetryExport holds where the gateway's request traces go as
	// one JSON value (store.telemetryExportValue: header values encrypted).
	// Only its own route reads and writes it; the generic settings routes
	// neither list nor accept it.
	SettingTelemetryExport SettingKey = "telemetry_export"
)

// SettingKeys returns every setting key this server writes on its own. The
// per-node network prefix is excluded: it names a family of keys, not one row.
func SettingKeys() []SettingKey {
	return []SettingKey{
		SettingNetwork,
		SettingImageCommunityCatalog,
		SettingTerminalPolicy,
		SettingDeploymentPolicy,
		SettingOrphanCleanup,
		SettingMCPEnabled,
		SettingTelemetryExport,
	}
}

// String makes a SettingKey usable where a plain string is required (the
// Setting row's primary key, event payloads, error messages).
func (k SettingKey) String() string { return string(k) }
