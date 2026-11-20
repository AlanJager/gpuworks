package messages

// Refusals of the plugin administration's lifecycle routes
// (/api/v1/plugins/{id}/restart, upgrade and rollback, and DELETE
// /api/v1/plugins/{id}, docs/reference/api.md).
const (
	// ErrorPluginDisabled: plugin {id} is disabled, so it cannot be
	// restarted; enable starts it.
	ErrorPluginDisabled = "PLUGIN_DISABLED"
	// ErrorPluginUninstallConfirmRequired: uninstalling {id} changes what
	// is enforced, and the request did not carry the digest of that impact
	// ({confirm}), or the impact changed since it was previewed; nothing
	// was removed. The counts: {held_nodes} holds released, {users_affected}
	// users whose roles or mappings it pushed, {caps_lifted} fleet caps
	// lifted, {features_locked} features locked, {banners} banners and
	// {overrides} overrides removed.
	ErrorPluginUninstallConfirmRequired = "PLUGIN_UNINSTALL_CONFIRM_REQUIRED"
	// ErrorPluginUpgradeRolledBack: the upgrade of {id} to {version} did
	// not pass its health gate ({cause_code}: the code of the failure,
	// else PLUGIN_TIMEOUT or PLUGIN_UNAVAILABLE), so {previous} runs again
	// with the data it had before the upgrade.
	ErrorPluginUpgradeRolledBack = "PLUGIN_UPGRADE_ROLLED_BACK"
	// ErrorPluginSnapshotFailed: the data of {id} could not be copied
	// before an upgrade; nothing was upgraded and the running version was
	// started again.
	ErrorPluginSnapshotFailed = "PLUGIN_SNAPSHOT_FAILED"
	// ErrorPluginNoPreviousVersion: {id} has no unpacked version to roll
	// back to.
	ErrorPluginNoPreviousVersion = "PLUGIN_NO_PREVIOUS_VERSION"
	// ErrorPluginSnapshotMissing: a rollback of {id} to {version} was asked
	// to restore its data, and no snapshot of that version's data is kept;
	// nothing changed.
	ErrorPluginSnapshotMissing = "PLUGIN_SNAPSHOT_MISSING"
	// ErrorPluginUpgradeInProgress: {id} is being upgraded; an install of
	// the same id is refused until the upgrade passes its health gate or
	// rolls back.
	ErrorPluginUpgradeInProgress = "PLUGIN_UPGRADE_IN_PROGRESS"
	// ErrorPluginUpgradeIDMismatch: the package uploaded to upgrade {id}
	// is the package {package_id}; nothing changed.
	ErrorPluginUpgradeIDMismatch = "PLUGIN_UPGRADE_ID_MISMATCH"
	// ErrorPluginUpgradeCancelled: the health gate of the upgrade of {id}
	// was ended early, by a disable or uninstall of {id} or by the
	// server's stop; it is the cause_code of PLUGIN_UPGRADE_ROLLED_BACK.
	ErrorPluginUpgradeCancelled = "PLUGIN_UPGRADE_CANCELLED"
)

// PluginAdminCodes lists the plugin lifecycle codes, for the check that the
// console carries text for each.
func PluginAdminCodes() []string {
	return []string{ErrorPluginDisabled, ErrorPluginUninstallConfirmRequired, ErrorPluginUpgradeRolledBack, ErrorPluginSnapshotFailed,
		ErrorPluginNoPreviousVersion, ErrorPluginSnapshotMissing, ErrorPluginUpgradeInProgress, ErrorPluginUpgradeIDMismatch,
		ErrorPluginUpgradeCancelled}
}
