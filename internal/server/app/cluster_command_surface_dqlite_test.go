//go:build linux && dqlite

package app_test

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The member-local cluster commands of a separately built server call these
// engine operations; see cluster_command_surface_test.go.
func TestClusterCommandsReachTheEngineOperations(t *testing.T) {
	var (
		_ = app.InitializeEnrollmentWithTier
		_ = app.ActivateDatabaseAddress
		_ = app.PrepareClusterJoin
		_ = app.VerifyClusterJoinTransport
		_ = app.RegisterClusterJoinOrigin
		_ = app.ReceiveClusterTransportPolicy
		_ = app.ReceiveClusterMaterials
		_ = app.ReceiveClusterCatalog
		_ = app.JoinCluster
		_ = app.ObserveClusterStatus
		_ = app.CheckClusterMaterialBootstrap
		_ = app.ClusterUpgradeStep
		_ = app.ClusterTransportPolicyPath
		_ = app.ClusterJoinTarget{}
		_ = app.ClusterJoinInfo{}
		_ = store.ReadRemoteClusterMaterials
		_ = store.ClusterIdentity{}
		_ = store.ClusterTransportMember{}
		_ = store.MaterialManifestRecord{}
		_ store.MaintenancePhaseState
	)
	if app.ErrBusinessStatusNotReady == nil {
		t.Fatal("the not-ready status error is nil")
	}
}
