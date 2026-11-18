package extapi

import (
	"slices"
	"sort"
	"testing"
)

// The permission vocabulary is part of schema 1: each permission names
// exactly the capability a server offers to serve it.
func TestPermissionCapabilityNamesTheServingCapability(t *testing.T) {
	want := map[string]string{
		"read:features":           "host.features",
		"read:fleet":              "host.fleet",
		"read:node_clocks":        "host.node_clocks",
		"read:host_identity":      "host.host_identity",
		"read:directory":          "host.directory",
		"read:usage":              "host.usage",
		"read:audit":              "host.audit",
		"read:attestations":       "host.attestations",
		"read:node_identity":      "host.node_identity",
		"read:node_inventory":     "host.nodes",
		"crypto:derive_key":       "host.derive_key",
		"events:node_fleet":       "host.fleet",
		"http:routes":             "routes",
		"http:public":             "routes",
		"http:uploads":            "routes",
		"metrics:expose":          "metrics.expose",
		"notify:raise":            "host.notify",
		"node:workloads":          "node.app",
		"node:jobs":               "node.app",
		"push:availability":       "table.availability",
		"push:banners":            "table.banners",
		"push:node_holds":         "table.node_holds",
		"push:fleet_capacity":     "table.fleet_capacity",
		"push:attestations":       "table.attestations",
		"push:insert_caps":        "table.insert_caps",
		"push:allowances":         "table.allowances",
		"push:prices":             "table.prices",
		"push:roles.core_actions": "table.roles.core_actions",
	}
	for p, c := range want {
		if got, ok := PermissionCapability(p); !ok || got != c {
			t.Errorf("PermissionCapability(%q) = %q, %v; want %q", p, got, ok, c)
		}
	}
	all := Permissions()
	if len(all) != len(want) {
		t.Errorf("Permissions() has %d entries, the vocabulary above %d: %v", len(all), len(want), all)
	}
	if !sort.StringsAreSorted(all) {
		t.Errorf("Permissions() is not sorted: %v", all)
	}
	for _, p := range all {
		c, ok := PermissionCapability(p)
		if !ok || !permissionPattern.MatchString(p) || !capabilityPattern.MatchString(c) {
			t.Errorf("%q: capability %q, %v; both must match their grammars", p, c, ok)
		}
	}
}

func TestPermissionCapabilityRefusesWhatTheVocabularyLacks(t *testing.T) {
	for _, p := range []string{
		"", "read", "read:", "read:fleets", "read:Fleet", "events:x", "events:node", "svc:provider.driver",
		"svc:notify.channel", "push:quota_limits", "push:", "http:html", "write:fleet", "crypto:sign", ":fleet",
	} {
		if c, ok := PermissionCapability(p); ok {
			t.Errorf("PermissionCapability(%q) = %q, true; want refused", p, c)
		}
	}
}

func TestReservedTableKindsArePrivilegedAndUndefined(t *testing.T) {
	priv := PrivilegedTableKinds()
	if !sort.StringsAreSorted(priv) || !sort.StringsAreSorted(ReservedTableKinds()) {
		t.Fatalf("privileged %v or reserved %v not sorted", priv, ReservedTableKinds())
	}
	for _, k := range ReservedTableKinds() {
		if !slices.Contains(priv, k) {
			t.Errorf("reserved kind %s is not privileged", k)
		}
		if TableSchemas(k) != nil {
			t.Errorf("reserved kind %s has a schema; move it out of the reserved list", k)
		}
	}
	// The returned slices are copies.
	priv[0] = "x"
	if PrivilegedTableKinds()[0] == "x" {
		t.Fatal("PrivilegedTableKinds returned the package's own slice")
	}
}
