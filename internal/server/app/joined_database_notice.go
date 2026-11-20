//go:build linux && dqlite

package app

import (
	"os"
	"strings"
)

// joinedDatabaseListenNotice reports why a joined process will not open its
// database address. An empty result means the directory still has Raft metadata
// and a log or snapshot, so this is not the identity-only case.
func joinedDatabaseListenNotice(raftDir string) string {
	entries, err := os.ReadDir(raftDir)
	if err != nil {
		return ""
	}
	meta, segment := false, false
	for _, entry := range entries {
		name := entry.Name()
		switch name {
		case "info.yaml", "cluster.yaml", "dqlite-lock":
		case "metadata1", "metadata2":
			meta = true
		default:
			if (len(name) == 33 && name[16] == '-') || strings.HasPrefix(name, "snapshot-") {
				segment = true
			}
		}
	}
	const effect = "database address will not listen and the material route will not be mounted"
	if !meta {
		return "persisted raft directory has only identity files; " + effect
	}
	if !segment {
		return "persisted raft directory has no raft log or snapshot; " + effect
	}
	return ""
}
