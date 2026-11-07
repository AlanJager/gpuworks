package main

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

func TestServerConfigAppliesExplicitPrivateRestrictionMode(t *testing.T) {
	if app.DefaultConfig().OutboundRestrictPrivateHosts {
		t.Fatal("private restrictions must default off")
	}
	for _, restrict := range []bool{false, true} {
		cfg := app.DefaultConfig()
		cfg.OutboundRestrictPrivateHosts = !restrict
		applyServerConfig(&cfg, serverconfig.File{OutboundRestrictPrivateHosts: restrict}, nil)
		if cfg.OutboundRestrictPrivateHosts != restrict {
			t.Fatalf("configured strict mode=%v, applied=%v", restrict, cfg.OutboundRestrictPrivateHosts)
		}
	}
}
