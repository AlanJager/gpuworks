package extapi

import (
	"slices"
	"sort"
	"strconv"
	"strings"
)

// PermissionVerbs is the closed set of permission verbs of schema 1. The
// object after the colon comes from a closed vocabulary per verb (see
// PermissionCapability); a table kind follows push.
var PermissionVerbs = []string{"crypto", "events", "http", "metrics", "node", "notify", "push", "read", "svc"}

// Capabilities that permissions name. A permission is usable only through
// its capability, so a manifest that holds the permission must list the
// capability in requires or uses; a server that does not offer it refuses
// a required one as incompatible and never serves an optional one.
const (
	CapHostFeatures     = "host.features"
	CapHostFleet        = "host.fleet"
	CapHostNodeClocks   = "host.node_clocks"
	CapHostHostIdentity = "host.host_identity"
	CapHostDirectory    = "host.directory"
	CapHostUsage        = "host.usage"
	CapHostAudit        = "host.audit"
	CapHostAttestations = "host.attestations"
	CapHostNodeIdentity = "host.node_identity"
	CapHostNodes        = "host.nodes"
	CapHostDeriveKey    = "host.derive_key"
	CapHostNotify       = "host.notify"
	CapHostConfig       = "host.config" // settings declarations and their values
	CapMetricsExpose    = "metrics.expose"
	CapRoutesWebSocket  = "routes.websocket" // a route that upgrades to WebSocket
)

// permissionObjects maps each verb except push to its closed set of
// objects and the capability each one names. A verb whose set is empty
// (svc) has no object yet: a later schema adds objects only beside a new
// capability.
var permissionObjects = map[string]map[string]string{
	"crypto": {"derive_key": CapHostDeriveKey},
	// node_fleet is the change feed of the fleet read, so it rides on the
	// same capability.
	"events":  {"node_fleet": CapHostFleet},
	"http":    {"public": CapRoutes, "routes": CapRoutes, "uploads": CapRoutes},
	"metrics": {"expose": CapMetricsExpose},
	"node":    {"jobs": CapNodeApp, "workloads": CapNodeApp},
	"notify":  {"raise": CapHostNotify},
	"read": {
		"attestations":   CapHostAttestations,
		"audit":          CapHostAudit,
		"directory":      CapHostDirectory,
		"features":       CapHostFeatures,
		"fleet":          CapHostFleet,
		"host_identity":  CapHostHostIdentity,
		"node_clocks":    CapHostNodeClocks,
		"node_identity":  CapHostNodeIdentity,
		"node_inventory": CapHostNodes,
		"usage":          CapHostUsage,
	},
	"svc": {},
}

// reservedTableKinds are table kinds reserved for privileged tables that no
// schema of this package defines yet. A manifest may ask to push them
// (push:<kind>, behind table.<kind>), so a certificate can grant them and a
// later server can offer them without a new manifest schema.
var reservedTableKinds = []string{"allowances", "insert_caps", "prices", "roles.core_actions"}

// privilegedTableKinds are the table kinds a package may push only with its
// certificate's or the host operator's grant.
var privilegedTableKinds = []string{"allowances", KindFleetCapacity, "insert_caps", "prices", "roles.core_actions"}

// PrivilegedTableKinds returns the table kinds a package may push only
// with a certificate grant, sorted.
func PrivilegedTableKinds() []string { return slices.Clone(privilegedTableKinds) }

// ReservedTableKinds returns the privileged table kinds reserved for a
// later schema, sorted.
func ReservedTableKinds() []string { return slices.Clone(reservedTableKinds) }

// PermissionCapability returns the capability permission p names, and
// false when p is not in the vocabulary of schema 1.
func PermissionCapability(p string) (string, bool) {
	verb, object, ok := strings.Cut(p, ":")
	if !ok {
		return "", false
	}
	if verb == "push" {
		if _, known := tableSchemas[object]; known || slices.Contains(reservedTableKinds, object) {
			return TableCapability(object), true
		}
		return "", false
	}
	c, ok := permissionObjects[verb][object]
	return c, ok
}

// Permissions returns every permission of schema 1, sorted.
func Permissions() []string {
	var out []string
	for verb, objects := range permissionObjects {
		for object := range objects {
			out = append(out, verb+":"+object)
		}
	}
	for kind := range tableSchemas {
		out = append(out, "push:"+kind)
	}
	for _, kind := range reservedTableKinds {
		out = append(out, "push:"+kind)
	}
	sort.Strings(out)
	return out
}

// checkPermissionVocabulary refuses a permission whose verb or object is
// outside the vocabulary.
func checkPermissionVocabulary(perms []string) *fieldError {
	for i, p := range perms {
		at := joinPath("permissions", strconv.Itoa(i))
		verb, _, _ := strings.Cut(p, ":")
		if f := oneOf(at, verb, PermissionVerbs...); f != nil {
			return f
		}
		if _, ok := PermissionCapability(p); !ok {
			return &fieldError{field: at, rule: RuleEnum}
		}
	}
	return nil
}

// validatePermissionCapabilities makes every permission's capability
// appear in requires or uses.
func (m *Manifest) validatePermissionCapabilities() *fieldError {
	for i, p := range m.Permissions {
		c, _ := PermissionCapability(p)
		if m.Requires[c] == 0 && m.Uses[c] == 0 {
			return &fieldError{field: joinPath("permissions", strconv.Itoa(i)), rule: RuleRelation}
		}
	}
	return nil
}
