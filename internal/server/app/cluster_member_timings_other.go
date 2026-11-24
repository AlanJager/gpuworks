//go:build !linux || !dqlite

package app

import (
	"context"
	"errors"
	"time"
)

// ClusterMemberTimings requires the cluster runtime, which this build lacks.
func ClusterMemberTimings(context.Context, Config, time.Duration) ([]ClusterMemberTiming, error) {
	return nil, errors.New("reading the members' timing requires the native Linux dqlite server build")
}
