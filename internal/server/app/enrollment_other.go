//go:build !linux || !dqlite

package app

import (
	"context"
	"errors"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func enrollmentPlatform() error {
	return errors.New("cluster enrollment requires the native Linux dqlite server build")
}

// ClusterRaftSession is the native joining database. This build cannot start it.
type ClusterRaftSession struct{}

func (s *ClusterRaftSession) ID() uint64 { return 0 }

func (s *ClusterRaftSession) Close() error {
	if s == nil {
		return nil
	}
	return enrollmentPlatform()
}

func InitializeEnrollment(context.Context, Config, string, string) (EnrollmentInfo, error) {
	return EnrollmentInfo{}, enrollmentPlatform()
}

func InitializeEnrollmentWithTier(context.Context, Config, string, string, storagetier.Tier) (EnrollmentInfo, error) {
	return EnrollmentInfo{}, enrollmentPlatform()
}

func PrepareClusterJoin(context.Context, Config, ClusterJoinTarget) (ClusterJoinInfo, error) {
	return ClusterJoinInfo{}, enrollmentPlatform()
}

func ReceiveClusterMaterials(context.Context, Config, ClusterJoinTarget, string) (ClusterJoinInfo, error) {
	return ClusterJoinInfo{}, enrollmentPlatform()
}

func ReceiveClusterCatalog(context.Context, Config, ClusterJoinTarget) error {
	return enrollmentPlatform()
}

func ReceiveClusterTransportPolicy(context.Context, Config, ClusterJoinTarget) error {
	return enrollmentPlatform()
}

func VerifyClusterJoinTransport(context.Context, Config, ClusterJoinTarget) error {
	return enrollmentPlatform()
}

func RegisterClusterJoinOrigin(context.Context, Config, ClusterJoinTarget) error {
	return enrollmentPlatform()
}

func PrepareClusterRaftIdentity(context.Context, Config, ClusterJoinTarget) (uint64, error) {
	return 0, enrollmentPlatform()
}

func ReconcileClusterRaftMarker(context.Context, Config, ClusterJoinTarget) error {
	return enrollmentPlatform()
}

func StartClusterRaft(context.Context, Config, ClusterJoinTarget) (*ClusterRaftSession, error) {
	return nil, enrollmentPlatform()
}

func StartPersistedClusterRaft(context.Context, Config) (*ClusterRaftSession, error) {
	return nil, enrollmentPlatform()
}

func OpenPersistedJoinedStore(context.Context, Config, *bus.Bus) (*store.Store, error) {
	return nil, enrollmentPlatform()
}

func JoinCluster(context.Context, Config, ClusterJoinTarget) (*ClusterRaftSession, ClusterCatchUp, error) {
	return nil, ClusterCatchUp{}, enrollmentPlatform()
}

func ConfirmClusterCatchUp(context.Context, []*cluster.Database) (string, string, uint64, error) {
	return "", "", 0, enrollmentPlatform()
}

func RemoveClusterMember(context.Context, *cluster.Database, uint64, func(context.Context, cluster.Member) bool, bool) error {
	return enrollmentPlatform()
}

func ReplaceClusterMember(context.Context, *cluster.Database, uint64, uint64, func(context.Context, cluster.Member) bool, bool) error {
	return enrollmentPlatform()
}
