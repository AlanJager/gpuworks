package messages

// Plugin process supervision and the host API (internal/server/pluginhost,
// docs/dev/extensions.md). Params name the package ({id}) where one is
// concerned. PLUGIN_ISOLATION_UNAVAILABLE is plugin.go's, shared with package
// verification. The codes a plugin's wire objects fail with are pkg/extapi's.
const (
	// ErrorPluginUnavailable: plugin {plugin} is not running, so a call to
	// it cannot be served now (the call never reached it; params also carry
	// its {state} for programs, which the text does not use), or a feature
	// it answers for ({feature}) is narrowed because it has been down
	// longer than its grace.
	ErrorPluginUnavailable = "PLUGIN_UNAVAILABLE"
	// ErrorPluginTimeout: plugin {id} did not answer within {timeout_s}
	// seconds; {state} is starting when the deadline passed while an
	// on-demand plugin was still starting, running otherwise.
	ErrorPluginTimeout = "PLUGIN_TIMEOUT"
	// ErrorPluginRouteFailed: plugin {id} had the request and the call
	// failed (it crashed or hung up), so the request may have been partly
	// applied; not retryable.
	ErrorPluginRouteFailed = "PLUGIN_ROUTE_FAILED"
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
	// ErrorPluginCapabilityNotOffered: the plugin cannot use a capability
	// version ({capability}@{version}): it chose one this server did not
	// offer or the package was not approved for, or it calls a host API
	// endpoint whose capability it did not negotiate at its handshake.
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
	// ErrorPluginMemoryExceeded: plugin {id}, running without a cgroup of
	// its own, held {rss_mib} MiB across its process group, more than the
	// watchdog allows for its {limit_mib} MiB limit, and was killed; it is
	// restarted after a backoff.
	ErrorPluginMemoryExceeded = "PLUGIN_MEMORY_EXCEEDED"
	// ErrorPluginHostNotReady: the plugin called the host API before its
	// handshake was accepted.
	ErrorPluginHostNotReady = "PLUGIN_HOST_NOT_READY"
	// ErrorPluginHostRateLimited: the plugin calls the host API, or reads
	// inventory pages, faster than {limit} a second.
	ErrorPluginHostRateLimited = "PLUGIN_HOST_RATE_LIMITED"
	// ErrorPluginHostRouteUnknown: the host API serves no {method} {path}.
	ErrorPluginHostRouteUnknown = "PLUGIN_HOST_ROUTE_UNKNOWN"
	// ErrorPluginHostFailed: the server failed to handle a host API call
	// ({detail}).
	ErrorPluginHostFailed = "PLUGIN_HOST_FAILED"
	// ErrorPluginStateOnDemand: on-demand plugin {id} may not push state
	// or declare features, because it is not running most of the time.
	// Install time only: a refused install also names the {permission} or
	// {feature}; a refused push carries {id} alone.
	ErrorPluginStateOnDemand = "PLUGIN_STATE_ON_DEMAND"
	// ErrorPluginHostNotPermitted: plugin {id} called a host API endpoint
	// whose {permission} its administrator did not approve.
	ErrorPluginHostNotPermitted = "PLUGIN_HOST_NOT_PERMITTED"
	// ErrorPluginHostArgumentInvalid: a host API request from plugin {id}
	// names an invalid {field} (a value out of its bounds or grammar).
	ErrorPluginHostArgumentInvalid = "PLUGIN_HOST_ARGUMENT_INVALID"
	// ErrorPluginRoleRequired: plugin {id} called a host API endpoint that
	// only a package granted the privileged {role} may call.
	ErrorPluginRoleRequired = "PLUGIN_ROLE_REQUIRED"
)

// PluginHostErrorCodes lists the codes above, for the check that the
// console carries text for each.
func PluginHostErrorCodes() []string {
	return []string{
		ErrorPluginUnavailable, ErrorPluginTimeout, ErrorPluginRouteFailed, ErrorPluginBusy, ErrorPluginStartFailed,
		ErrorPluginHandshakeTimeout, ErrorPluginHandshakeRefused, ErrorPluginHandshakeMismatch,
		ErrorPluginCapabilityNotOffered, ErrorPluginCrashed, ErrorPluginKilled, ErrorPluginCrashloop, ErrorPluginUnhealthy,
		ErrorPluginMemoryExceeded,
		ErrorPluginHostNotReady, ErrorPluginHostRateLimited, ErrorPluginHostRouteUnknown, ErrorPluginHostFailed,
		ErrorPluginStateOnDemand, ErrorPluginHostNotPermitted, ErrorPluginHostArgumentInvalid,
		ErrorPluginRoleRequired,
	}
}
