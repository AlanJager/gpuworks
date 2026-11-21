//go:build !linux || !dqlite

package app

import (
	"context"
	"errors"
	"time"
)

// errClusterBackupUnsupported is what a build without the cluster runtime
// answers: it is never a cluster member, so it has nothing to back up.
var errClusterBackupUnsupported = errors.New("cluster backup and restore require a Linux build with the cluster runtime")

// ClusterBackupOptions says where a backup goes and how long it may take.
type ClusterBackupOptions struct {
	Dir        string
	ConfigFile string
	Timeout    time.Duration
}

// ClusterRestoreOptions bounds a restore.
type ClusterRestoreOptions struct {
	ConfigFile string
	Timeout    time.Duration
}

// BackupCluster requires the cluster runtime.
func BackupCluster(context.Context, Config, ClusterBackupOptions) (ClusterBackup, error) {
	return ClusterBackup{}, errClusterBackupUnsupported
}
