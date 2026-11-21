//go:build !linux || !dqlite

package app

import (
	"context"
	"time"
)

// observeCluster in a build without the cluster runtime: one server.
func observeCluster(_ context.Context, _ Config, _ time.Duration) (ClusterObservation, error) {
	return ClusterObservation{ObservedAt: time.Now()}, nil
}
