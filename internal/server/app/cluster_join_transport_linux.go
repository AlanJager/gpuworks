//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
)

// VerifyClusterJoinTransport is a bounded observation, never a membership or
// readiness transition. No durable success marker survives the observed state.
func VerifyClusterJoinTransport(ctx context.Context, cfg Config, target ClusterJoinTarget) error {
	return verifyClusterJoinTransport(ctx, cfg, target, cluster.ProbeMember)
}

func verifyClusterJoinTransport(ctx context.Context, cfg Config, target ClusterJoinTarget, probe func(context.Context, string, string, *identity.Trust, []byte, []byte, identity.TransportPolicy) error) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return withClusterJoinMaterials(ctx, cfg, target, func(dir string, info ClusterJoinInfo, key []byte, material joinMaterialSnapshot) error {
		policy, err := identity.LoadTransportPolicy(filepath.Join(dir, "transport-policy.json"))
		if err != nil {
			return err
		}
		if policy.SourceRevision == 0 || policy.ClusterID != info.ClusterID || policy.CAFingerprint != info.CAFingerprint {
			return errors.New("joining policy requires matching source identity and revision")
		}
		trust, err := identity.NewTrust([]byte(material.Secrets.CACertificatePEM), info.ClusterID, info.CAFingerprint, time.Now())
		if err != nil {
			return err
		}
		client, err := enrollment.NewClient(info.EnrollmentURL, info.ClusterID, info.CAFingerprint, info.Protocol)
		if err != nil {
			return err
		}
		defer client.Close()
		certificate := []byte(material.CertificatePEM)
		source, err := client.FetchTransportPolicy(ctx, certificate, key)
		if err != nil {
			return err
		}
		if source.SourceRevision != policy.SourceRevision || source.ClusterID != policy.ClusterID || source.CAFingerprint != policy.CAFingerprint || !slices.Equal(source.AllowedKeys, policy.AllowedKeys) || !slices.Equal(source.RevokedKeys, policy.RevokedKeys) {
			return errors.New("joining policy differs from current source; receive a fresh policy before verification")
		}
		members, err := client.FetchClusterMembership(ctx, certificate, key)
		if err != nil {
			return err
		}
		if members.SourceRevision != source.SourceRevision {
			return errors.New("membership and authorization source revisions differ")
		}
		for _, member := range members.Members {
			if err := ctx.Err(); err != nil {
				return err
			}
			probeMember, err := shouldProbeJoinMember(info, member)
			if err != nil {
				return err
			}
			if !probeMember {
				// A restarted join command runs before its own database listener.
				// The source must name this exact persisted member identity;
				// probing our offline address would make retry impossible.
				continue
			}
			if err := probe(ctx, member.Address, member.KeyFingerprint, trust, certificate, key, policy); err != nil {
				return err
			}
		}
		finalPolicy, err := client.FetchTransportPolicy(ctx, certificate, key)
		if err != nil {
			return err
		}
		finalMembers, err := client.FetchClusterMembership(ctx, certificate, key)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(source, finalPolicy) || !reflect.DeepEqual(members, finalMembers) {
			return errors.New("cluster authorization or membership changed during transport verification")
		}
		return ctx.Err()
	})
}

func shouldProbeJoinMember(info ClusterJoinInfo, member enrollment.ClusterMember) (bool, error) {
	if member.Address == info.Address || member.KeyFingerprint == info.KeyFingerprint {
		if member.Address != info.Address || member.KeyFingerprint != info.KeyFingerprint {
			return false, errors.New("source membership conflicts with the persisted joining member identity")
		}
		return false, nil
	}
	return true, nil
}
