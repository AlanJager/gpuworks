package app

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/diagnostics"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

// unclassifiedDirectory words why a data directory's collection cannot say
// whether it is a cluster member's.
func unclassifiedDirectory(err error) error {
	return fmt.Errorf("whether this installation is a control-plane cluster member could not be read: %w", err)
}

// unclassifiedCluster is the cluster of a data directory whose cluster
// markers cannot be read, and nil for every other: the collection of a
// single server made without the server (gpuworks support-bundle) carries
// the failed item for the first, and is otherwise left as it is.
func unclassifiedCluster(cfg Config) diagnostics.Cluster {
	if _, err := selfinstall.ClusterMember(cfg.DataDir); err == nil {
		return nil
	}
	return newClusterDiagnostics(cfg)
}

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
