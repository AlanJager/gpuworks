//go:build !linux || !dqlite

package app

import (
	"context"
	"errors"
	"log/slog"

	"gpuworks.ai/gpuworks/internal/server/diagnostics"
)

// newClusterDiagnostics: a build without the cluster engine has no members
// to collect from.
func newClusterDiagnostics(Config) diagnostics.Cluster { return nil }

// CollectClusterMemberDiagnostics: a cluster member's files are read by a
// build that carries the cluster runtime.
func CollectClusterMemberDiagnostics(context.Context, ClusterMemberDiagnosticsRequest, *slog.Logger) (diagnostics.Bundle, string, error) {
	return diagnostics.Bundle{}, "", errors.New("this data directory belongs to a control-plane cluster member, and this build carries no cluster runtime; run the support-bundle command of the cluster's own gpuworks")
}
