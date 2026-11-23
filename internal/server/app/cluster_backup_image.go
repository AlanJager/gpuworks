package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

// ErrClusterBackupUnavailable reports that no consistent image could be read:
// no leader answered, the read failed, or leadership moved, on every attempt.
var ErrClusterBackupUnavailable = errors.New("cluster backup could not read a consistent database image")

// clusterBackupRead is how a backup reads the database: who leads, with a
// barrier of that member; the database image from a member; and another
// barrier of that member.
type clusterBackupRead struct {
	leader func(context.Context) (cluster.QuorumProof, string, error)
	image  func(ctx context.Context, leader string) ([]byte, error)
	proof  func(ctx context.Context, leader string) (cluster.QuorumProof, error)
	// pause is the wait before the second attempt, doubled before the
	// third; zero means half a second.
	pause time.Duration
}

// clusterBackupAttempts is how often the image is read before the backup
// gives up.
const clusterBackupAttempts = 3

// clusterBackupImage reads the database image between two barriers of the
// same leader in the same term, which shows that member led throughout, so
// the image holds only committed writes: all of them up to the first barrier
// and none after the second.
//
// The leadership can move while it reads: an election, or a handover before
// a member is replaced. Then no member answers as the leader for a moment,
// the read from the former leader fails, or the second barrier names another
// term or leader. Each of these is one attempt; the image is read again,
// three times in all, and the error of the last attempt is returned.
func clusterBackupImage(ctx context.Context, read clusterBackupRead) ([]byte, cluster.QuorumProof, cluster.QuorumProof, error) {
	pause := read.pause
	if pause <= 0 {
		pause = 500 * time.Millisecond
	}
	var last error
	for attempt := 0; attempt < clusterBackupAttempts; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(pause)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, cluster.QuorumProof{}, cluster.QuorumProof{}, errors.Join(ctx.Err(), last)
			case <-timer.C:
			}
			pause *= 2
		}
		before, leader, err := read.leader(ctx)
		if err != nil {
			last = err
			continue
		}
		image, err := read.image(ctx, leader)
		if err != nil {
			last = fmt.Errorf("%w: %s: %w", ErrClusterBackupUnavailable, leader, err)
			continue
		}
		after, err := read.proof(ctx, leader)
		if err == nil && after.Term == before.Term && after.LeaderID == before.LeaderID && after.Index >= before.Index {
			return image, before, after, nil
		}
		if err != nil {
			last = fmt.Errorf("%w: %s: %w", ErrClusterBackupUnavailable, leader, err)
		} else {
			last = fmt.Errorf("%w: the leadership moved while the database was read from %s", ErrClusterBackupUnavailable, leader)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, cluster.QuorumProof{}, cluster.QuorumProof{}, errors.Join(err, last)
	}
	return nil, cluster.QuorumProof{}, cluster.QuorumProof{}, fmt.Errorf("%w (%d attempts)", last, clusterBackupAttempts)
}
