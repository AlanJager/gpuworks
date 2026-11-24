package app

import "gpuworks.ai/gpuworks/internal/server/diagnostics"

// ClusterMemberDiagnosticsRequest is a collection made on a control-plane
// cluster member without its server (gpuworks support-bundle): the member's
// own log, stall reports and database role records are read from its
// directories, and the other members are asked for theirs with this member's
// certificate. The cluster database is not opened, so the collection can be
// made while the cluster has no leader; the records it holds (settings,
// nodes, deployments) are then missing, and the manifest says so.
type ClusterMemberDiagnosticsRequest struct {
	DataDir string
	LogFile string
	// WorkDir is where the collection is staged and packed.
	WorkDir string
	Request diagnostics.BundleRequest
	// Local collects this member alone.
	Local bool
}
