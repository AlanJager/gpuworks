package extpkg_test

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

func TestEveryPermissionHasARisk(t *testing.T) {
	for _, p := range extapi.Permissions() {
		switch r := extpkg.PermissionRisk(p); r {
		case extpkg.RiskLow, extpkg.RiskElevated, extpkg.RiskHigh:
		default:
			t.Errorf("%s: risk %q", p, r)
		}
	}
	for p, want := range map[string]string{
		"read:host_identity": extpkg.RiskHigh, "push:node_holds": extpkg.RiskHigh, "read:directory": extpkg.RiskHigh,
		"push:banners": extpkg.RiskLow, "http:public": extpkg.RiskElevated, "read:fleet": extpkg.RiskElevated,
	} {
		if got := extpkg.PermissionRisk(p); got != want {
			t.Errorf("%s: risk %s, want %s", p, got, want)
		}
	}
	if got := extpkg.PermissionRisk("read:something_new"); got != extpkg.RiskHigh {
		t.Errorf("an unclassified permission is %s, want high", got)
	}
}
