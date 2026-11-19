package messages

// Refusals of the plugin administration's lifecycle routes
// (/api/v1/plugins/{id}/restart and DELETE /api/v1/plugins/{id},
// docs/reference/api.md).
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
)

// PluginAdminCodes lists the plugin lifecycle codes, for the check that the
// console carries text for each.
func PluginAdminCodes() []string {
	return []string{ErrorPluginDisabled, ErrorPluginUninstallConfirmRequired}
}
