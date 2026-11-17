package extpkg

import (
	"fmt"
	"slices"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// AuthorityRole is the privileged role of a namespace's authority package:
// only the package the certificate names in authority_id may claim it.
const AuthorityRole = "availability_authority"

// privilegedTables are the table kinds a package may push only with its
// certificate's (or the host operator's) grant (ADR 0071, decision 5).
var privilegedTables = []string{"allowances", "fleet_capacity", "insert_caps", "prices", "roles.core_actions"}

// PrivilegedTables returns the table kinds that need a certificate grant,
// sorted.
func PrivilegedTables() []string { return slices.Clone(privilegedTables) }

// pushedTables returns the table kinds the manifest's permissions push,
// sorted (the manifest's permissions are).
func pushedTables(m *extapi.Manifest) []string {
	var out []string
	for _, p := range m.Permissions {
		if kind, ok := strings.CutPrefix(p, "push:"); ok {
			out = append(out, kind)
		}
	}
	return out
}

// FeatureNamespace is the namespace of feature key k declared by a package
// of publisher: the publisher's name when k starts with "<publisher>.",
// otherwise the bare namespace "", which only the project publisher may be
// granted. A third party's key outside its own name therefore always lands
// in "", which its certificate cannot grant.
func FeatureNamespace(publisher, k string) string {
	if strings.HasPrefix(k, publisher+".") {
		return publisher
	}
	return ""
}

func grantMissing(grant, value string) error {
	return messages.NewProductError(messages.ErrorPluginGrantMissing,
		fmt.Sprintf("the certificate does not grant %s %q", grant, value),
		map[string]any{"grant": grant, "value": value})
}

// checkGrants is step 8: everything the manifest claims is granted.
func checkGrants(m *extapi.Manifest, g extapi.CertGrants) error {
	for _, f := range m.Features {
		if ns := FeatureNamespace(m.Publisher, f.Key); !slices.Contains(g.FeatureNS, ns) {
			return grantMissing("feature_ns", f.Key)
		}
	}
	for _, r := range m.Roles {
		if !slices.Contains(g.Roles, r) {
			return grantMissing("role", r)
		}
		if r == AuthorityRole && g.AuthorityID != m.ID {
			return grantMissing("authority", m.ID)
		}
	}
	for _, kind := range pushedTables(m) {
		if slices.Contains(privilegedTables, kind) && !slices.Contains(g.Tables, kind) {
			return grantMissing("table", kind)
		}
	}
	if m.UI != nil && m.UI.Mode == extapi.UIModeModule && !g.UIModule {
		return grantMissing("ui_module", m.UI.Entry)
	}
	return nil
}

// ApprovalHash is what an administrator approves for a package: the digest
// of its permissions, roles, privileged tables, devices, console bundle
// mode, isolation and host setup steps. An upgrade with the same hash asks
// for nothing new.
func ApprovalHash(v *Verified) (extapi.Digest, error) {
	m := v.Manifest
	tables := []string{}
	for _, kind := range pushedTables(m) {
		if slices.Contains(privilegedTables, kind) {
			tables = append(tables, kind)
		}
	}
	devices, uiMode := []string{}, ""
	if m.Runtime != nil {
		devices = append(devices, m.Runtime.Devices...)
	}
	if m.UI != nil {
		uiMode = m.UI.Mode
	}
	steps := []any{}
	for _, s := range m.HostSetup {
		steps = append(steps, map[string]any{"type": s.Type, "vendor_id": s.VendorID, "product_id": s.ProductID})
	}
	return extapi.DigestValue(map[string]any{
		"permissions": strs(m.Permissions), "roles": strs(m.Roles), "tables": strs(tables), "devices": strs(devices),
		"ui_mode": uiMode, "isolation": v.Grants.MinIsolation, "host_setup": steps,
	})
}

func strs(list []string) []any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}
