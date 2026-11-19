package messages

// Refusals of the plugin package host (internal/server/extpkg): reading,
// verifying, installing and removing signed plugin packages, and importing
// trust updates and revocation lists. The failures of the wire formats
// themselves (PLUGIN_WIRE_*, PLUGIN_SIGNATURE_*, PLUGIN_MANIFEST_TOO_NEW,
// PLUGIN_STATE_TOO_NEW, ...) carry pkg/extapi's codes. The verification
// order is docs/reference/plugin-protocol.md.
const (
	// ErrorPluginPackageMalformed: the package is not a gzip-compressed tar,
	// or a member ({member}) is a link, a special file, an unsafe or
	// repeated name, or a required member is missing ({reason}).
	ErrorPluginPackageMalformed = "PLUGIN_PACKAGE_MALFORMED"
	// ErrorPluginPackageTooLarge: a member ({member}) takes the package past
	// its size limit ({max} bytes).
	ErrorPluginPackageTooLarge = "PLUGIN_PACKAGE_TOO_LARGE"
	// ErrorPluginCertRevoked: the publisher certificate that signed the
	// package is revoked ({publisher}, {serial}, {keyid}).
	ErrorPluginCertRevoked = "PLUGIN_CERT_REVOKED"
	// ErrorPluginPackageRevoked: this build of the package is revoked by its
	// manifest digest ({id}, {version}); {replacement} names the version to
	// install instead, when the revocation names one.
	ErrorPluginPackageRevoked = "PLUGIN_PACKAGE_REVOKED"
	// ErrorPluginManifestMismatch: manifest.json is not the payload
	// manifest.sig signs.
	ErrorPluginManifestMismatch = "PLUGIN_MANIFEST_MISMATCH"
	// ErrorPluginPublisherMismatch: the manifest names {publisher}, but the
	// certificate is for {cert_publisher}.
	ErrorPluginPublisherMismatch = "PLUGIN_PUBLISHER_MISMATCH"
	// ErrorPluginIDNotGranted: the certificate of {publisher} does not
	// cover package id {id}.
	ErrorPluginIDNotGranted = "PLUGIN_ID_NOT_GRANTED"
	// ErrorPluginCreatedOutsideWindow: the package was created at {created},
	// outside the certificate's signing window [{not_before}, {not_after}].
	ErrorPluginCreatedOutsideWindow = "PLUGIN_CREATED_OUTSIDE_WINDOW"
	// ErrorPluginGrantMissing: the package claims something its certificate
	// does not grant: {grant} (feature_ns, role, authority, table or
	// ui_module) {value}.
	ErrorPluginGrantMissing = "PLUGIN_GRANT_MISSING"
	// ErrorPluginFileMismatch: a member ({member}) does not match the signed
	// file list ({reason}: unlisted, missing, size or sha256).
	ErrorPluginFileMismatch = "PLUGIN_FILE_MISMATCH"
	// ErrorPluginBinaryMissing: the package has a server part but not
	// exactly one executable for {os}/{arch} ({count} found).
	ErrorPluginBinaryMissing = "PLUGIN_BINARY_MISSING"
	// ErrorPluginIncompatible: package {id} requires {capability} at
	// version {version}, and this server offers {offered} (0: not at all);
	// {min_core_hint} names the server release the package was built for.
	// It is also the administrator banner of an enabled package that stopped
	// verifying for this reason.
	ErrorPluginIncompatible = "PLUGIN_INCOMPATIBLE"
	// ErrorPluginPolicyRefused: plugin policy refuses the package ({rule},
	// {value}): the site's policy in server.yaml (rule is the policy key),
	// or this server's own policy on routes (rule is the permission the
	// route needs, value the route).
	ErrorPluginPolicyRefused = "PLUGIN_POLICY_REFUSED"
	// ErrorPluginIsolationUnavailable: package {id} needs {isolation} and
	// this host cannot confine it so.
	ErrorPluginIsolationUnavailable = "PLUGIN_ISOLATION_UNAVAILABLE"
	// ErrorPluginBelowFloor: version {version} of {id} is below the lowest
	// version this server may install ({floor}).
	ErrorPluginBelowFloor = "PLUGIN_BELOW_FLOOR"
	// ErrorPluginTrustReplayed: a trust update or revocation list of
	// {issuer} with serial {serial} is not newer than the one applied
	// ({highest}).
	ErrorPluginTrustReplayed = "PLUGIN_TRUST_REPLAYED"
	// ErrorPluginTrustConflict: a trust update adds root key {keyid}, which
	// this server already knows as a different key.
	ErrorPluginTrustConflict = "PLUGIN_TRUST_CONFLICT"
	// ErrorPluginNotInstalled: no package {id} is installed.
	ErrorPluginNotInstalled = "PLUGIN_NOT_INSTALLED"
	// ErrorPluginVersionConflict: version {version} of {id} is installed
	// with other contents; a build is never replaced under the same version.
	ErrorPluginVersionConflict = "PLUGIN_VERSION_CONFLICT"
	// ErrorPluginFeatureTaken: package {id} declares feature {feature},
	// which {owner} already provides: a module compiled into this server
	// (its name) or another installed package (its id). A key keeps its
	// owner; nothing was installed.
	ErrorPluginFeatureTaken = "PLUGIN_FEATURE_TAKEN"
	// ErrorPluginNeedsNewerCore: an index offers {id} {version}, which needs
	// a newer server ({min_core_hint}) because it requires {capability}.
	ErrorPluginNeedsNewerCore = "PLUGIN_NEEDS_NEWER_CORE"
)

// Plugin administration (/api/v1/plugins, docs/reference/api.md).
const (
	// ErrorPluginApprovalRequired: installing {id} {version} needs the
	// administrator's approval of what it asks for ({permissions},
	// {roles}); the request must carry {approve_hash}. Nothing was
	// installed. The params also carry the diff from the installed version
	// {from_version} (empty on a first install): {permissions_added} and
	// {permissions_removed} (permission=risk), an _added/_removed pair for
	// each of roles, capabilities, features, routes, host_setup and
	// devices, and {ui_mode} and {isolation} (old->new when changed).
	ErrorPluginApprovalRequired = "PLUGIN_APPROVAL_REQUIRED"
	// ErrorPluginBroken: installed package {id} no longer verifies, so it
	// cannot be enabled; see its state.
	ErrorPluginBroken = "PLUGIN_BROKEN"
	// ErrorPluginsUnavailable: this server runs no plugin host, so plugins
	// cannot be managed here.
	ErrorPluginsUnavailable = "PLUGINS_UNAVAILABLE"
)

// Host setup: the root steps a package's host_setup declares
// (docs/dev/extensions.md).
const (
	// ErrorPluginHostSetupRequired: package {id} declares host setup steps
	// that root has not applied on this host, or applied for other steps;
	// its server part does not start until an administrator runs
	// {command}.
	ErrorPluginHostSetupRequired = "PLUGIN_HOST_SETUP_REQUIRED"
	// ErrorPluginHostSetupUnsupported: package {id} declares host setup
	// steps, which this platform cannot apply; nothing was installed.
	ErrorPluginHostSetupUnsupported = "PLUGIN_HOST_SETUP_UNSUPPORTED"
)

// PluginHostSetupErrorCodes lists the host setup codes, for the check that
// the console carries text for each.
func PluginHostSetupErrorCodes() []string {
	return []string{ErrorPluginHostSetupRequired, ErrorPluginHostSetupUnsupported}
}
