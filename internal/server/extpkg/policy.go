package extpkg

import (
	"fmt"
	"slices"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Policy is the site's plugin policy (server.yaml plugins.policy and
// plugins.allow_unsandboxed). It can only narrow what trust allows: no
// setting lets in a package that verification would otherwise refuse.
type Policy struct {
	// AllowPublishers, when not empty, are the only publishers whose
	// packages install.
	AllowPublishers []string
	// DenyPackages, DenyPermissions and DenyRoles refuse any package with
	// that id, permission or claimed role.
	DenyPackages    []string
	DenyPermissions []string
	DenyRoles       []string
	// RequireIsolation "landlock" refuses any server part on a host that
	// cannot confine it with Landlock.
	RequireIsolation string
	// AllowUnsandboxed are package ids of third parties allowed to run
	// unconfined on a host without Landlock although their certificate asks
	// for it.
	AllowUnsandboxed []string
	// AutoApproveUnchanged lets Host.Upgrade apply a package whose approval
	// hash equals the approved one without a new approval
	// (plugins.policy.auto_approve_unchanged). Verification ignores it.
	AutoApproveUnchanged bool
}

// Rules of ErrorPluginPolicyRefused.
const (
	PolicyPublisher  = "allow_publishers"
	PolicyPackage    = "deny_packages"
	PolicyPermission = "deny_permissions"
	PolicyRole       = "deny_roles"
)

func policyRefused(rule, value string) error {
	return messages.NewProductError(messages.ErrorPluginPolicyRefused,
		fmt.Sprintf("site plugin policy %s refuses %q", rule, value),
		map[string]any{"rule": rule, "value": value})
}

// check is step 11. hostIsolation is what this host can apply.
func (p Policy) check(m *extapi.Manifest, v *Verified, hostIsolation string) error {
	if len(p.AllowPublishers) > 0 && !slices.Contains(p.AllowPublishers, m.Publisher) {
		return policyRefused(PolicyPublisher, m.Publisher)
	}
	if slices.Contains(p.DenyPackages, m.ID) {
		return policyRefused(PolicyPackage, m.ID)
	}
	for _, perm := range m.Permissions {
		if slices.Contains(p.DenyPermissions, perm) {
			return policyRefused(PolicyPermission, perm)
		}
	}
	for _, role := range m.Roles {
		if slices.Contains(p.DenyRoles, role) {
			return policyRefused(PolicyRole, role)
		}
	}
	if m.Runtime == nil || hostIsolation == extapi.IsolationLandlock {
		return nil
	}
	isolationUnavailable := func() error {
		return messages.NewProductError(messages.ErrorPluginIsolationUnavailable,
			fmt.Sprintf("%s must run under Landlock, which this host lacks", m.ID),
			map[string]any{"id": m.ID, "isolation": extapi.IsolationLandlock})
	}
	if p.RequireIsolation == extapi.IsolationLandlock {
		return isolationUnavailable()
	}
	// Without Landlock the signature is the only defence: a third party's
	// package runs only if its certificate allows that or the operator
	// listed it.
	if m.Publisher != extapi.ProjectPublisher && v.Grants.MinIsolation == extapi.IsolationLandlock && !slices.Contains(p.AllowUnsandboxed, m.ID) {
		return isolationUnavailable()
	}
	return nil
}
