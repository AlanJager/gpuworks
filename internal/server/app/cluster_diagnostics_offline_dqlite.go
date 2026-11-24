//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"time"

	"gpuworks.ai/gpuworks/internal/debuglog"
	"gpuworks.ai/gpuworks/internal/server/diagnostics"
)

// CollectClusterMemberDiagnostics builds the collection and answers its
// archive.
func CollectClusterMemberDiagnostics(ctx context.Context, in ClusterMemberDiagnosticsRequest, log *slog.Logger) (diagnostics.Bundle, string, error) {
	cfg := Config{DataDir: in.DataDir, LogFile: in.LogFile}
	members := newClusterDiagnostics(cfg)
	if members == nil {
		return diagnostics.Bundle{}, "", errors.New("the data directory is not a control-plane cluster member's")
	}
	if in.Local {
		members = localMember{members}
	}
	m := diagnostics.NewManager(filepath.Join(in.DataDir, "diagnostics"), debuglog.New(slog.DiscardHandler), offlineNodes{}, noRecordsScope{}, log)
	m.SetSources(clusterMemberSources{files: diagnosticsSources{logFile: in.LogFile, dataDir: in.DataDir}})
	m.SetCluster(members)
	if in.WorkDir != "" {
		m.SetBundleDir(in.WorkDir)
	}
	return m.CollectNow(ctx, in.Request)
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

func (l localMember) Members(ctx context.Context) ([]diagnostics.ClusterMember, error) {
	members, err := l.Cluster.Members(ctx)
	if err != nil {
		return nil, err
	}
	var out []diagnostics.ClusterMember
	for _, member := range members {
		if member.Self {
			out = append(out, member)
		}
	}
	return out, nil
}
