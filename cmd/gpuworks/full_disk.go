package main

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/diagnostics"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// nearlyFullBytes is the room below which a data directory is taken to be
// the reason the server stopped: SQLite cannot even size its shared-memory
// file there, and says only "disk I/O error".
const nearlyFullBytes = 1 << 20

// filesystemUsage is diagnostics.FilesystemUsage, replaced in tests.
var filesystemUsage = diagnostics.FilesystemUsage

// explainFullDisk adds to why the server stopped that the disk holding its
// data directory is full, when it is: the error the database gives then (a
// disk I/O error opening the WAL, for one) does not say so. statfs writes
// nothing, so it still answers on that disk. Any other error is left as it is.
func explainFullDisk(err error, dataDir string) error {
	if err == nil || dataDir == "" {
		return err
	}
	total, avail, statErr := filesystemUsage(dataDir)
	if statErr != nil || (avail >= nearlyFullBytes && !store.IsStorageFull(err)) {
		return err
	}
	return fmt.Errorf("%w (the disk holding the data directory %s has %s free of %s: free space on it, then start the server again)",
		err, dataDir, sizeText(avail), sizeText(total))
}

func sizeText(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value, suffix := float64(n), "B"
	for _, s := range []string{"KiB", "MiB", "GiB", "TiB"} {
		if value < unit {
			break
		}
		value, suffix = value/unit, s
	}
	return fmt.Sprintf("%.1f %s", value, suffix)
}
