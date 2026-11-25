//go:build !linux || !dqlite

package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"gpuworks.ai/gpuworks/internal/server/diagnostics"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
	"gpuworks.ai/gpuworks/internal/version"
)

// newClusterDiagnostics: a build without the cluster engine has no members
// to collect from. A directory whose cluster markers cannot be read is still
// not taken for a single server's: its collection says what could not be
// read.
func newClusterDiagnostics(cfg Config) diagnostics.Cluster {
	if _, err := selfinstall.ClusterMember(cfg.DataDir); err != nil {
		return unclassifiedMember(cfg, err)
	}
	return nil
}

// unclassifiedMember is the cluster of a data directory whose cluster
// markers could not be read, for the reason err.
func unclassifiedMember(cfg Config, err error) diagnostics.Cluster {
	return unclassifiedFiles{cfg: cfg, err: unclassifiedDirectory(err)}
}

// unclassifiedFiles is such a directory in this build: its membership is the
// error, and its own part is what its log holds.
type unclassifiedFiles struct {
	cfg Config
	err error
}

func (u unclassifiedFiles) Members(context.Context) ([]diagnostics.ClusterMember, error) {
	return nil, u.err
}
func (u unclassifiedFiles) Self() diagnostics.MemberSource { return u }
func (u unclassifiedFiles) OpenMemberPart(context.Context, diagnostics.ClusterMember, time.Time, time.Time) (diagnostics.MemberAnswer, error) {
	return diagnostics.MemberAnswer{}, u.err
}
func (u unclassifiedFiles) Facts(context.Context) diagnostics.MemberFacts {
	return diagnostics.MemberFacts{Version: version.String(), LogFile: u.cfg.LogFile}
}
func (u unclassifiedFiles) LogFiles() []string {
	return diagnosticsSources{logFile: u.cfg.LogFile}.ServerLogFiles()
}
func (u unclassifiedFiles) StallDir() string              { return diagnostics.StallReportDir(u.cfg.DataDir) }
func (u unclassifiedFiles) Status(context.Context) string { return "" }

// CollectClusterMemberDiagnostics: a cluster member's files are read by a
// build that carries the cluster runtime.
func CollectClusterMemberDiagnostics(context.Context, ClusterMemberDiagnosticsRequest, *slog.Logger) (diagnostics.Bundle, string, error) {
	return diagnostics.Bundle{}, "", errors.New("this data directory belongs to a control-plane cluster member, and this build carries no cluster runtime; run the support-bundle command of the cluster's own gpuworks")
}
