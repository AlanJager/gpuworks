//go:build linux && dqlite

package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"time"

	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func clusterInstallMaterialBootstrap(ctx context.Context, req clusterInstallNodeRequest) error {
	dial, finalPolicy, localKey, err := clusterInstallObserverTransport(req, false)
	if err != nil {
		return err
	}
	leader, leaderKey, err := cluster.ObserveRaftLeader(ctx, net.JoinHostPort(req.Host, fmt.Sprint(req.Database)), dial)
	if err != nil {
		return err
	}
	pinned := func(ctx context.Context, target string) (net.Conn, error) {
		if target != leader.Address {
			return nil, errors.New("bootstrap material SQL refused a changed leader endpoint")
		}
		conn, key, err := dial(ctx, target)
		if err == nil && key != leaderKey {
			err = errors.New("bootstrap material leader key changed")
		}
		return conn, err
	}
	first, err := cluster.ReadQuorumProof(ctx, leader.Address, pinned)
	if err != nil {
		return err
	}
	if first.LeaderID != leader.ID || !clusterInstallThreeVoters(req, first) {
		return errors.New("bootstrap material native voters differ from the installation")
	}
	var ownID uint64
	for _, member := range first.Members {
		if member.Address == net.JoinHostPort(req.Host, fmt.Sprint(req.Database)) {
			ownID = member.ID
		}
	}
	if err := finalPolicy(); err != nil {
		return err
	}
	read := func(ctx context.Context, expected store.ClusterIdentity, requesterKey string) (store.MaintenancePhaseState, []store.ClusterTransportMember, []store.MaterialManifestRecord, error) {
		if expected.ClusterID != req.ClusterID || expected.CAFingerprint != req.CAFingerprint || requesterKey != localKey {
			return store.MaintenancePhaseState{}, nil, nil, errors.New("bootstrap material requester differs from the installation")
		}
		phase, members, pointers, readErr := store.ReadRemoteClusterMaterials(ctx, expected, leader, leaderKey, requesterKey, "gpuworks.db", pinned)
		if err := errors.Join(readErr, finalPolicy()); err != nil {
			return store.MaintenancePhaseState{}, nil, nil, err
		}
		return phase, members, pointers, nil
	}
	data := req.Dir + "/data"
	if err := app.CheckClusterMaterialBootstrap(ctx, app.Config{DataDir: data, DBPath: data + "/gpuworks.db"}, req.Host, ownID, req.Hosts, req.Database, req.Enrollment, read); err != nil {
		return err
	}
	last, err := cluster.ReadQuorumProof(ctx, leader.Address, pinned)
	if err != nil {
		return err
	}
	// Installed checks legitimately advance the commit index and grant counters.
	if last.LeaderID != first.LeaderID || last.Term != first.Term || last.ConfigurationIndex != first.ConfigurationIndex || !slices.Equal(last.Members, first.Members) {
		return errors.New("bootstrap material native configuration changed during installed verification")
	}
	return errors.Join(ctx.Err(), finalPolicy())
}

func clusterInstallWaitMaterialBootstrap(ctx context.Context, check func(context.Context) error) error {
	var last error
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, last)
		}
		last = check(ctx)
		if last == nil {
			return ctx.Err()
		}
		// Poll an actual pre-formal material prerequisite. This delay is not a
		// substitute for its evidence, and never retries healthy business.
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), last)
		case <-time.After(500 * time.Millisecond):
		}
	}
}
