package app

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
)

// RecordedStorageTier is the storage tier the installation in dataDir runs:
// the one its enrollment configuration records. An installation that records
// none, or is no cluster member, runs the standard tier. A command reads it
// before it starts the database node; the enrollment bundle's own reader
// validates everything else in that file. A member that is still joining has
// no enrollment configuration yet and names its tier itself.
func RecordedStorageTier(dataDir string) (storagetier.Tier, error) {
	name, _, err := recordedStorageTierIn(filepath.Join(dataDir, "cluster-enrollment", "config.json"))
	if err != nil {
		return "", err
	}
	return storagetier.Parse(name)
}

func recordedStorageTierIn(path string) (string, bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer file.Close()
	var recorded struct {
		StorageTier string `json:"storage_tier"`
	}
	if err := json.NewDecoder(io.LimitReader(file, 128<<10)).Decode(&recorded); err != nil {
		return "", false, errors.New("read recorded storage tier: invalid cluster configuration")
	}
	return recorded.StorageTier, true, nil
}
