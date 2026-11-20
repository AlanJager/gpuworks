//go:build linux && dqlite

package app

import (
	"context"
	"errors"

	"gpuworks.ai/gpuworks/internal/server/enrollment"
)

// RegisterClusterJoinOrigin publishes the persisted member's HTTPS endpoint
// through the member that actually served an authenticated membership read.
// The subsequent write pins that same member key, including across redirects
// or a changed TLS endpoint.
func RegisterClusterJoinOrigin(ctx context.Context, cfg Config, target ClusterJoinTarget) error {
	return withClusterJoinMaterials(ctx, cfg, target, func(_ string, info ClusterJoinInfo, key []byte, material joinMaterialSnapshot) error {
		if info.EnrollmentOrigin == "" {
			return nil
		}
		client, err := enrollment.NewClient(info.EnrollmentURL, info.ClusterID, info.CAFingerprint, info.Protocol)
		if err != nil {
			return err
		}
		defer client.Close()
		certificate := []byte(material.CertificatePEM)
		_, peerKey, err := client.FetchClusterMembershipWithPeerKey(ctx, certificate, key)
		if err != nil {
			return err
		}
		if err := client.RegisterClusterEnrollmentOrigin(ctx, certificate, key, info.EnrollmentOrigin, peerKey); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(errors.New("origin registration outcome may be committed; retry with the same member identity"), err)
		}
		return nil
	})
}
