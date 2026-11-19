package messages

// Codes of the core's pushed state: refusals of a plugin's push, the notices
// the core itself attaches when a plugin is down, and refusals of an
// administrator's restriction override (ADR 0071 decisions 8 to 10). The
// wire format's own failures (PLUGIN_STATE_REJECTED, PLUGIN_STATE_TOO_LARGE,
// ...) are declared by pkg/extapi.
const (
	// ErrorPluginStateNotPermitted: the plugin may not push this kind; it
	// lacks the approved permission or the certificate grant ({kind},
	// {needs}).
	ErrorPluginStateNotPermitted = "PLUGIN_STATE_NOT_PERMITTED"
	// ErrorPluginStateNotOwner: a pushed row names something the plugin
	// does not answer for: a feature key it is not the authority for, a code
	// its manifest does not list ({kind}, {field}, {row}).
	ErrorPluginStateNotOwner = "PLUGIN_STATE_NOT_OWNER"
	// ErrorPluginStateStale: the push's generation is not above the one the
	// server holds or is about to commit ({kind}, {generation}, {held}).
	ErrorPluginStateStale = "PLUGIN_STATE_STALE"
	// ErrorPluginStateConnectionReplaced: plugin {id} pushed {kind} over the
	// connection of a process the server has since replaced, or of a
	// package version no longer started.
	ErrorPluginStateConnectionReplaced = "PLUGIN_STATE_CONNECTION_REPLACED"
	// ErrorPluginStateNotSaved: the server could not persist the push, so it
	// did not apply it; the plugin pushes again ({kind}).
	ErrorPluginStateNotSaved = "PLUGIN_STATE_NOT_SAVED"
	// ErrorPluginDown: an administrator banner while a plugin is down; what
	// it pushed stays enforced ({plugin}, {since}).
	ErrorPluginDown = "PLUGIN_DOWN"
	// ErrorPluginClockCorrected: an administrator banner after extclock was
	// corrected ({from}, {to}, {by}).
	ErrorPluginClockCorrected = "PLUGIN_CLOCK_CORRECTED"
	// ErrorPluginClockCorrectionTooLarge: a clock correction moves extclock
	// further than one correction may ({max_days}).
	ErrorPluginClockCorrectionTooLarge = "PLUGIN_CLOCK_CORRECTION_TOO_LARGE"
	// ErrorPluginOverrideRejected: a restriction override cannot be applied
	// ({plugin}, {kind}, {entry}, {rule}: not_restrict, no_entry, action,
	// not_looser, reason or superseded).
	ErrorPluginOverrideRejected = "PLUGIN_OVERRIDE_REJECTED"
	// ErrorPluginOverrideSuperseded: the plugin pushed the overridden entry
	// again, so its new decision replaced the override ({plugin}, {kind},
	// {entry}).
	ErrorPluginOverrideSuperseded = "PLUGIN_OVERRIDE_SUPERSEDED"
	// ErrorPluginOverrideNotFound: no override {id} is in force; it was
	// removed, superseded by the plugin's own push, or dropped with the
	// plugin's uninstall.
	ErrorPluginOverrideNotFound = "PLUGIN_OVERRIDE_NOT_FOUND"
)
