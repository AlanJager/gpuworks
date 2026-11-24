//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"time"

	"gpuworks.ai/gpuworks/internal/server/enrollment"
)

// ClusterMemberTimings asks every other member of this installation's
// cluster which storage tier it runs. The members and their enrollment
// listeners come from the leader's membership, as the status report learns
// them; each is then asked on its own listener with this member's key. A
// member that cannot be asked, or whose program does not say, carries the
// reason instead of a tier: a caller that needs every member's timing must
// not guess one.
func ClusterMemberTimings(ctx context.Context, cfg Config, timeout time.Duration) ([]ClusterMemberTiming, error) {
	if timeout <= 0 {
		return nil, errors.New("reading the members' timing requires a positive timeout")
	}
	report, err := readClusterStatus(cfg)
	if err != nil {
		return nil, err
	}
	if report.clusterID == "not recorded" || report.pin == "" || len(report.cert) == 0 || len(report.key) == 0 {
		return nil, errors.New("this installation holds no cluster member identity")
	}
	facts, ok := statusQuorumFetch(ctx, report, timeout)
	if !ok {
		return nil, errors.New("the cluster's membership could not be read from any member's enrollment listener")
	}
	var timings []ClusterMemberTiming
	for _, member := range facts.Members {
		if member.ID == report.id {
			continue
		}
		timing := ClusterMemberTiming{ID: member.ID, Address: member.Address}
		timing.Tier, timing.Unread = memberTiming(ctx, report, member.EnrollmentOrigin, timeout)
		timings = append(timings, timing)
	}
	return timings, nil
}

func memberTiming(ctx context.Context, report clusterStatus, origin string, timeout time.Duration) (string, string) {
	if !enrollment.CanonicalEnrollmentOrigin(origin) {
		return "", "the membership names no enrollment listener for it"
	}
	client, err := enrollment.NewClient(origin+enrollmentCertificatePath, report.clusterID, report.pin, report.protocol)
	if err != nil {
		return "", err.Error()
	}
	defer client.Close()
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	response, err := client.ClusterUpgrade(callCtx, report.cert, report.key, enrollment.ClusterUpgradeStatus)
	switch {
	case err != nil:
		return "", "it did not answer: " + err.Error()
	case response.StorageTier == "":
		return "", "its program does not say which storage tier it runs"
	}
	return response.StorageTier, ""
}
