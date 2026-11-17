package messages

// Plugin process supervision and the host API (internal/server/pluginhost,
// docs/dev/extensions.md). Params name the package ({id}) where one is
// concerned. PLUGIN_ISOLATION_UNAVAILABLE is plugin.go's, shared with package
// verification. The codes a plugin's wire objects fail with are pkg/extapi's.
const (
	// ErrorPluginUnavailable: plugin {plugin} is not running, so a call to
	// it cannot be served now, or a feature it answers for ({feature}) is
	// narrowed because it has been down longer than its grace.
	ErrorPluginUnavailable = "PLUGIN_UNAVAILABLE"
	// ErrorPluginBusy: the plugin already serves as many server calls as it
	// may ({limit}); retry shortly.
	ErrorPluginBusy = "PLUGIN_BUSY"
	// ErrorPluginStartFailed: the plugin process could not be started
	// ({detail}).
	ErrorPluginStartFailed = "PLUGIN_START_FAILED"
	// ErrorPluginHandshakeTimeout: the plugin did not answer initialize
	// within {timeout_s} seconds.
	ErrorPluginHandshakeTimeout = "PLUGIN_HANDSHAKE_TIMEOUT"
	// ErrorPluginHandshakeRefused: the plugin refused initialize with its
	// own code ({plugin_code}, {reason}).
	ErrorPluginHandshakeRefused = "PLUGIN_HANDSHAKE_REFUSED"
	// ErrorPluginHandshakeMismatch: the binary answered for another package
	// build than the installed one ({field}: {want}, got {got}).
	ErrorPluginHandshakeMismatch = "PLUGIN_HANDSHAKE_MISMATCH"
	// ErrorPluginCapabilityNotOffered: the plugin chose a capability
	// version ({capability}@{version}) this server did not offer or the
	// package was not approved for.
	ErrorPluginCapabilityNotOffered = "PLUGIN_CAPABILITY_NOT_OFFERED"
	// ErrorPluginCrashed: the plugin process exited unexpectedly with
	// {exit_code}; it is restarted after a backoff.
	ErrorPluginCrashed = "PLUGIN_CRASHED"
	// ErrorPluginKilled: the plugin process was ended by {signal}; it is
	// restarted after a backoff.
	ErrorPluginKilled = "PLUGIN_KILLED"
	// ErrorPluginCrashloop: the plugin crashed {crashes} times within
	// {window_s} seconds; it is retried every {retry_s} seconds.
	ErrorPluginCrashloop = "PLUGIN_CRASHLOOP"
	// ErrorPluginUnhealthy: the plugin missed {misses} health probes in a
	// row and is restarted.
	ErrorPluginUnhealthy = "PLUGIN_UNHEALTHY"
	// ErrorPluginHostNotReady: the plugin called the host API before its
	// handshake was accepted.
	ErrorPluginHostNotReady = "PLUGIN_HOST_NOT_READY"
	// ErrorPluginHostRateLimited: the plugin calls the host API faster than
	// {limit} requests a second.
	ErrorPluginHostRateLimited = "PLUGIN_HOST_RATE_LIMITED"
	// ErrorPluginHostRouteUnknown: the host API serves no {method} {path}.
	ErrorPluginHostRouteUnknown = "PLUGIN_HOST_ROUTE_UNKNOWN"
	// ErrorPluginHostFailed: the server failed to handle a host API call
	// ({detail}).
	ErrorPluginHostFailed = "PLUGIN_HOST_FAILED"
	// ErrorPluginStateOnDemand: an on-demand plugin may not push state,
	// because it is not running most of the time.
	ErrorPluginStateOnDemand = "PLUGIN_STATE_ON_DEMAND"
)

// PluginHostErrorCodes lists the codes above, for the check that the
// console carries text for each.
func PluginHostErrorCodes() []string {
	return []string{
		ErrorPluginUnavailable, ErrorPluginBusy, ErrorPluginStartFailed,
		ErrorPluginHandshakeTimeout, ErrorPluginHandshakeRefused, ErrorPluginHandshakeMismatch,
		ErrorPluginCapabilityNotOffered, ErrorPluginCrashed, ErrorPluginKilled, ErrorPluginCrashloop, ErrorPluginUnhealthy,
		ErrorPluginHostNotReady, ErrorPluginHostRateLimited, ErrorPluginHostRouteUnknown, ErrorPluginHostFailed,
		ErrorPluginStateOnDemand,
	}
}
