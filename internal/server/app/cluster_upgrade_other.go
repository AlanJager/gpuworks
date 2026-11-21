//go:build !linux || !dqlite

package app

import (
	"context"
	"errors"
	"time"

	"gpuworks.ai/gpuworks/internal/server/enrollment"
)

// ClusterUpgradeStep requires the cluster runtime, which this build lacks.
func ClusterUpgradeStep(context.Context, Config, string, time.Duration) (enrollment.ClusterUpgradeResponse, error) {
	return enrollment.ClusterUpgradeResponse{}, errors.New("a cluster upgrade step requires the native Linux dqlite server build")
}
