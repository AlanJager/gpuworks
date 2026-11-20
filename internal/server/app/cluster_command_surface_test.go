package app_test

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

// The cluster commands are registered by a separately built server through
// cli.Options.Commands. This test names what they reach in every build, so
// renaming or removing one of these is a visible contract change; the engine
// functions that exist only in a clustered build are named in
// cluster_command_surface_dqlite_test.go.
func TestClusterCommandsReachTheInstallationSurface(t *testing.T) {
	var (
		_ func(string) (storagetier.Tier, error) = app.RecordedStorageTier
		_ app.ClusterCatchUp
		_ = selfinstall.RenderUnit
		_ = selfinstall.UnitConfig{}
		_ = selfinstall.NewIO
		_ *selfinstall.IO
		_ error = selfinstall.ErrNoTerminal
		_       = dto.ClusterInvitationResponse{}
	)
	if selfinstall.DefaultDir == "" {
		t.Fatal("the default install directory is empty")
	}
	tier, err := app.RecordedStorageTier(t.TempDir())
	if err != nil || tier != storagetier.Standard {
		t.Fatalf("an installation that records no tier = %q, %v; want standard", tier, err)
	}
}
