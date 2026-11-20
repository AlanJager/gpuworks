//go:build !linux || !dqlite

package app

import (
	"context"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

func ActivateDatabaseAddress(context.Context, Config, string) (cluster.Member, error) {
	return cluster.Member{}, enrollmentPlatform()
}
