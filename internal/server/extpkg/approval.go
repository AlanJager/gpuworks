package extpkg

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// PermissionChange is one permission an upgrade adds or drops, with the
// risk class the console shows next to it.
type PermissionChange struct {
	Permission string
	Risk       string
}

// Change is a single value an upgrade changes; From equals To when it
// stays the same.
type Change struct{ From, To string }

// Changed reports whether the value differs.
func (c Change) Changed() bool { return c.From != c.To }

// ApprovalDiff is what an administrator approves when a package replaces
// the installed one: everything the new version asks for that the old one
// did not, and what it no longer asks for. A first install is the diff from
// nothing. Every list is sorted.
type ApprovalDiff struct {
	// FromVersion is the installed version, empty for a first install;
	// ToVersion is the incoming one.
	FromVersion, ToVersion string
	// HashChanged reports that the incoming approval hash differs from the
	// approved one: the package asks for something not yet approved.
	HashChanged bool

	PermissionsAdded, PermissionsRemoved []PermissionChange
	RolesAdded, RolesRemoved             []string
	// Capabilities are "name@version", required and optional alike.
	CapabilitiesAdded, CapabilitiesRemoved []string
	// Features are feature keys.
	FeaturesAdded, FeaturesRemoved []string
	// Routes are "METHOD pattern access".
	RoutesAdded, RoutesRemoved []string
	// HostSetup are "type vendor_id:product_id".
	HostSetupAdded, HostSetupRemoved []string
	DevicesAdded, DevicesRemoved     []string
	UIMode, Isolation                Change
}

// DiffApproval compares what installed package old asks for with what next
// does; a nil old is a first install.
func DiffApproval(old, next *Verified) ApprovalDiff {
	o, n := approvalFacts(old), approvalFacts(next)
	d := ApprovalDiff{ToVersion: next.Manifest.Version, UIMode: Change{o.uiMode, n.uiMode}, Isolation: Change{o.isolation, n.isolation}}
	if old != nil {
		d.FromVersion = old.Manifest.Version
	}
	added, removed := setDiff(o.permissions, n.permissions)
	d.PermissionsAdded, d.PermissionsRemoved = withRisk(added), withRisk(removed)
	d.RolesAdded, d.RolesRemoved = setDiff(o.roles, n.roles)
	d.CapabilitiesAdded, d.CapabilitiesRemoved = setDiff(o.capabilities, n.capabilities)
	d.FeaturesAdded, d.FeaturesRemoved = setDiff(o.features, n.features)
	d.RoutesAdded, d.RoutesRemoved = setDiff(o.routes, n.routes)
	d.HostSetupAdded, d.HostSetupRemoved = setDiff(o.hostSetup, n.hostSetup)
	d.DevicesAdded, d.DevicesRemoved = setDiff(o.devices, n.devices)
	return d
}

// facts are the parts of a package an approval diff compares.
type facts struct {
	permissions, roles, capabilities, features, routes, hostSetup, devices []string
	uiMode, isolation                                                      string
}

func approvalFacts(v *Verified) facts {
	var f facts
	if v == nil {
		return f
	}
	m := v.Manifest
	f.permissions, f.roles, f.isolation = m.Permissions, m.Roles, v.Grants.MinIsolation
	caps := map[string]int{}
	for name, ver := range m.Uses {
		caps[name] = ver
	}
	for name, ver := range m.Requires {
		caps[name] = ver
	}
	for name, ver := range caps {
		f.capabilities = append(f.capabilities, fmt.Sprintf("%s@%d", name, ver))
	}
	for _, d := range m.Features {
		f.features = append(f.features, d.Key)
	}
	for _, r := range m.Routes {
		f.routes = append(f.routes, r.Method+" "+r.Pattern+" "+r.Access)
	}
	for _, s := range m.HostSetup {
		f.hostSetup = append(f.hostSetup, s.Type+" "+s.VendorID+":"+s.ProductID)
	}
	if m.Runtime != nil {
		f.devices = m.Runtime.Devices
	}
	if m.UI != nil {
		f.uiMode = m.UI.Mode
	}
	return f
}

// setDiff returns what next has that old lacks and the reverse, each
// sorted and without duplicates.
func setDiff(old, next []string) (added, removed []string) {
	in := func(list []string, s string) bool { return slices.Contains(list, s) }
	for _, s := range next {
		if !in(old, s) && !in(added, s) {
			added = append(added, s)
		}
	}
	for _, s := range old {
		if !in(next, s) && !in(removed, s) {
			removed = append(removed, s)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

func withRisk(perms []string) []PermissionChange {
	var out []PermissionChange
	for _, p := range perms {
		out = append(out, PermissionChange{Permission: p, Risk: PermissionRisk(p)})
	}
	return out
}

// Params are the diff as ErrorPluginApprovalRequired carries it: every key
// is always present, lists are comma-separated (permissions as
// "permission=risk"), and a changed single value is "from->to" (empty when
// it stays the same).
func (d ApprovalDiff) Params() map[string]any {
	perms := func(list []PermissionChange) string {
		parts := make([]string, len(list))
		for i, p := range list {
			parts[i] = p.Permission + "=" + p.Risk
		}
		return strings.Join(parts, ",")
	}
	change := func(c Change) string {
		if !c.Changed() {
			return ""
		}
		return c.From + "->" + c.To
	}
	j := func(list []string) string { return strings.Join(list, ",") }
	return map[string]any{
		"from_version":      d.FromVersion,
		"permissions_added": perms(d.PermissionsAdded), "permissions_removed": perms(d.PermissionsRemoved),
		"roles_added": j(d.RolesAdded), "roles_removed": j(d.RolesRemoved),
		"capabilities_added": j(d.CapabilitiesAdded), "capabilities_removed": j(d.CapabilitiesRemoved),
		"features_added": j(d.FeaturesAdded), "features_removed": j(d.FeaturesRemoved),
		"routes_added": j(d.RoutesAdded), "routes_removed": j(d.RoutesRemoved),
		"host_setup_added": j(d.HostSetupAdded), "host_setup_removed": j(d.HostSetupRemoved),
		"devices_added": j(d.DevicesAdded), "devices_removed": j(d.DevicesRemoved),
		"ui_mode": change(d.UIMode), "isolation": change(d.Isolation),
	}
}

// ApprovalNeeded is the cause inside an ErrorPluginApprovalRequired
// refusal, for a caller that shows the diff itself (errors.As).
type ApprovalNeeded struct {
	Hash extapi.Digest
	Diff ApprovalDiff
}

func (a *ApprovalNeeded) Error() string {
	return fmt.Sprintf("%s needs approval %s", a.Diff.ToVersion, a.Hash)
}

// approvalRequired is the refusal that names what an install or upgrade
// asks the administrator to approve: the hash, the full permissions and
// roles of the incoming package, and the diff from what is installed.
func approvalRequired(m *extapi.Manifest, hash extapi.Digest, diff ApprovalDiff) error {
	params := diff.Params()
	params["id"], params["version"], params["approve_hash"] = m.ID, m.Version, string(hash)
	params["permissions"], params["roles"] = strings.Join(m.Permissions, ","), strings.Join(m.Roles, ",")
	return messages.WrapProductError(messages.ErrorPluginApprovalRequired,
		fmt.Sprintf("installing %s %s needs approval %s", m.ID, m.Version, hash),
		params, &ApprovalNeeded{Hash: hash, Diff: diff})
}
