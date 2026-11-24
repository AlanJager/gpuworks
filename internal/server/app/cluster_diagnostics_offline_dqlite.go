//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"

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
