package extpkg

// Risk classes of a permission, what the console shows next to it when an
// administrator approves a package. The server decides them so the console
// makes no judgement of its own.
const (
	RiskLow      = "low"      // touches only the package's own surface
	RiskElevated = "elevated" // reads fleet-wide facts or exposes the package more widely
	RiskHigh     = "high"     // reads identities or secrets, or restricts what users and nodes may do
)

// permissionRisks classifies every permission of manifest schema 1;
// TestEveryPermissionHasARisk keeps it complete.
var permissionRisks = map[string]string{
	"crypto:derive_key":       RiskElevated,
	"events:node_fleet":       RiskElevated,
	"http:public":             RiskElevated,
	"http:routes":             RiskLow,
	"http:uploads":            RiskLow,
	"metrics:expose":          RiskLow,
	"node:jobs":               RiskHigh,
	"node:workloads":          RiskHigh,
	"notify:raise":            RiskLow,
	"push:allowances":         RiskHigh,
	"push:attestations":       RiskLow,
	"push:availability":       RiskHigh,
	"push:banners":            RiskLow,
	"push:fleet_capacity":     RiskHigh,
	"push:insert_caps":        RiskHigh,
	"push:node_holds":         RiskHigh,
	"push:prices":             RiskElevated,
	"push:roles.core_actions": RiskHigh,
	"read:attestations":       RiskElevated,
	"read:audit":              RiskHigh,
	"read:directory":          RiskHigh,
	"read:features":           RiskLow,
	"read:fleet":              RiskElevated,
	"read:host_identity":      RiskHigh,
	"read:node_clocks":        RiskLow,
	"read:node_identity":      RiskHigh,
	"read:node_inventory":     RiskElevated,
	"read:usage":              RiskElevated,
}

// PermissionRisk is the risk class of permission p. A permission this
// server does not classify is high: approving what the server cannot
// judge must look as serious as it may be.
func PermissionRisk(p string) string {
	if r, ok := permissionRisks[p]; ok {
		return r
	}
	return RiskHigh
}
