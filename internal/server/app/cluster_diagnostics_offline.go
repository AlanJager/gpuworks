package app

import (
	"context"
	"errors"
	"io"
	"time"

	"gpuworks.ai/gpuworks/internal/server/diagnostics"
)

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

// errClusterRecordsOffline is why a member's offline collection holds no
// records.
var errClusterRecordsOffline = errors.New("the records are in the cluster database, which a collection made without the server does not open; collect them from the console while the cluster serves")

// clusterMemberSources are a member's files, without its database.
type clusterMemberSources struct{ files diagnosticsSources }

func (clusterMemberSources) OpenNodeFiles(context.Context, string, []string) (io.ReadCloser, error) {
	return nil, errClusterRecordsOffline
}

func (clusterMemberSources) InstanceLogs(context.Context, string, time.Time, time.Time, io.Writer, int64) (bool, error) {
	return false, errClusterRecordsOffline
}

func (clusterMemberSources) InstanceNodes([]string) ([]string, error)     { return nil, nil }
func (clusterMemberSources) DeploymentInstances([]uint) ([]string, error) { return nil, nil }

func (clusterMemberSources) Snapshot(context.Context, diagnostics.SnapshotScope) (map[string]any, error) {
	return nil, errClusterRecordsOffline
}

func (s clusterMemberSources) ServerLogFiles() []string            { return s.files.ServerLogFiles() }
func (s clusterMemberSources) ServerDirs() []diagnostics.ServerDir { return s.files.ServerDirs() }
func (s clusterMemberSources) Version() string                     { return s.files.Version() }

// noRecordsScope answers for a collection that names no deployment or node.
type noRecordsScope struct{}

func (noRecordsScope) DeploymentNodes([]uint) ([]string, error) { return nil, nil }
func (noRecordsScope) UnknownDeployment(ids []uint) (uint, bool) {
	if len(ids) > 0 {
		return ids[0], true
	}
	return 0, false
}

func (noRecordsScope) UnknownNode(uuids []string) (string, bool) {
	if len(uuids) > 0 {
		return uuids[0], true
	}
	return "", false
}

// localMember narrows a cluster to the member that collects.
type localMember struct{ diagnostics.Cluster }

func (l localMember) Members(ctx context.Context) []diagnostics.ClusterMember {
	var out []diagnostics.ClusterMember
	for _, member := range l.Cluster.Members(ctx) {
		if member.Self {
			out = append(out, member)
		}
	}
	return out
}
