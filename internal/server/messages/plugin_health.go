package messages

// Health of the installed plugins: the administrator banners the plugin
// host raises from the plugins' states, and the results of the plugins
// diagnostics check (docs/dev/extensions.md, 监管). A crashlooping plugin's
// banner reuses PLUGIN_CRASHLOOP with the params its status carries, and an
// incompatible package's banner reuses PLUGIN_INCOMPATIBLE with the params
// of its refusal.
const (
	// ErrorPluginRevoked: an administrator banner while enabled plugin {id}
	// is revoked (its build or its certificate); it does not run, and what
	// it restricted stays in force.
	ErrorPluginRevoked = "PLUGIN_REVOKED"
	// CheckPluginsOK: no enabled plugin is crashlooping, failed, broken,
	// revoked or incompatible.
	CheckPluginsOK = "CHECK_PLUGINS_OK"
	// CheckPluginsFailing: the enabled plugins {ids} (comma-separated)
	// cannot run: crashlooping, failed, broken or revoked.
	CheckPluginsFailing = "CHECK_PLUGINS_FAILING"
	// CheckPluginsIncompatible: the enabled plugins {ids}
	// (comma-separated) need capabilities this server does not offer.
	CheckPluginsIncompatible = "CHECK_PLUGINS_INCOMPATIBLE"
)

// PluginHealthCodes lists the codes above, for the check that the console
// carries text for each.
func PluginHealthCodes() []string {
	return []string{ErrorPluginRevoked, CheckPluginsOK, CheckPluginsFailing, CheckPluginsIncompatible}
}
