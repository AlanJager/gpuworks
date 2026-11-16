// Package version carries build-time version metadata for both binaries.
package version

// These are overridden at build time via -ldflags.
var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
	// Channel is the distribution channel this build follows for upgrades and
	// official catalog updates: master builds are "nightly", v* tags "release"
	// (ADR 0040).
	Channel = "nightly"
	// Edition names the build for display: in `gpuworks edition`, /healthz,
	// the server build info, the usage report and support bundles. A
	// separately built server sets it through its command line options before
	// any subcommand runs; nothing in this tree behaves differently by edition.
	Edition = "community"
)

// String returns a human-readable version line.
func String() string {
	return Version + " (" + Commit + ", " + BuildDate + ")"
}
