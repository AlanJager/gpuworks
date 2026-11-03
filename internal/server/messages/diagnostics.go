package messages

// Diagnostics codes: debug sessions and diagnostics collections (package
// diagnostics), refused before anything starts.
const (
	// ErrorDebugSessionInvalid: a debug session request is out of range;
	// {field} is the setting (duration or max_bytes), {max} its upper bound
	// in that setting's unit (seconds or bytes).
	ErrorDebugSessionInvalid = "DEBUG_SESSION_INVALID"
	// ErrorDebugSessionOpen: a debug session ({session}) is already open;
	// it is stopped before another starts.
	ErrorDebugSessionOpen = "DEBUG_SESSION_OPEN"
	// ErrorDiagnosticsScopeUnknown: a debug session or collection names a deployment or node
	// ({id}) that does not exist.
	ErrorDiagnosticsScopeUnknown = "DIAGNOSTICS_SCOPE_UNKNOWN"
	// ErrorDiagnosticsBundleInvalid: a diagnostics collection request is out
	// of range; {field} is window (at most {max} seconds) or scope (at most
	// {max} nodes and instances).
	ErrorDiagnosticsBundleInvalid = "DIAGNOSTICS_BUNDLE_INVALID"
	// ErrorDiagnosticsBundleRunning: a collection ({bundle}) is being built;
	// one runs at a time.
	ErrorDiagnosticsBundleRunning = "DIAGNOSTICS_BUNDLE_RUNNING"
	// ErrorDiagnosticsUnavailable: this server runs without diagnostics.
	ErrorDiagnosticsUnavailable = "DIAGNOSTICS_UNAVAILABLE"
	// ErrorDiagnosticsBundleNotFound: no collection {bundle} is kept (or it is
	// not ready to download).
	ErrorDiagnosticsBundleNotFound = "DIAGNOSTICS_BUNDLE_NOT_FOUND"
)
