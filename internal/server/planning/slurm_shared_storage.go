package planning

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// SlurmSharedStorage is the model storage suggested for a Slurm cluster: a
// mounted shared directory at Path, named Name, for the nodes labelled
// LabelKey=LabelValue (the entry's nodes, including the ones the cluster
// adds later).
type SlurmSharedStorage struct {
	Name, Path, LabelKey, LabelValue string
}

// SlurmSharedStorageFor says whether to suggest registering the cluster's
// shared filesystem as model storage, and with what: only for an entry made
// from a cloud cluster whose shared directory the cloud's driver knows
// (sharedDir, provider.Service.HPCSharedModelDir; "" when it knows none),
// that is not revoked, has joined nodes, and none of whose nodes has a
// shared storage yet (hasShared).
func SlurmSharedStorageFor(entry store.SlurmCluster, sharedDir string, joined int, hasShared bool) (SlurmSharedStorage, bool) {
	if entry.RevokedAt != nil || entry.ProviderID == nil || sharedDir == "" || joined == 0 || hasShared || entry.UUID == "" {
		return SlurmSharedStorage{}, false
	}
	return SlurmSharedStorage{Name: slurmStorageName(entry.Name), Path: sharedDir, LabelKey: nodelabel.KeySlurmCluster, LabelValue: entry.UUID}, true
}

// slurmStorageName is a storage name made from the entry's: lower case
// letters, digits and dashes, at most 63 characters, ending in "-models".
func slurmStorageName(entry string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(entry) {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		switch {
		case ok:
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	base := strings.TrimRight(b.String(), "-")
	if len(base) > 56 {
		base = strings.TrimRight(base[:56], "-")
	}
	if base == "" {
		return "slurm-models"
	}
	return base + "-models"
}
