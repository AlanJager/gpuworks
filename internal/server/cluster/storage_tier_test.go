package cluster

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
)

func TestDatabaseConfigResolvesItsStorageTier(t *testing.T) {
	previous := processStorageTier.Load()
	t.Cleanup(func() { processStorageTier.Store(previous) })
	processStorageTier.Store(nil)

	if got := (DatabaseConfig{}).storageTier(); got != storagetier.Standard {
		t.Fatalf("no tier anywhere = %q, want standard", got)
	}
	UseStorageTier(storagetier.SlowStorage)
	if got := (DatabaseConfig{}).storageTier(); got != storagetier.SlowStorage {
		t.Fatalf("process tier = %q, want slow-storage", got)
	}
	if got := (DatabaseConfig{StorageTier: storagetier.Standard}).storageTier(); got != storagetier.Standard {
		t.Fatalf("configured tier = %q, want the configuration's own over the process's", got)
	}
	UseStorageTier("")
	if got := (DatabaseConfig{}).storageTier(); got != storagetier.Standard {
		t.Fatalf("process tier cleared = %q, want standard", got)
	}
	err := (DatabaseConfig{Dir: "d", Name: "db", Address: "127.0.0.1:9001", StorageTier: "glacial"}).validate()
	if err == nil || !strings.Contains(err.Error(), `unknown storage tier "glacial"`) {
		t.Fatalf("unknown tier validated: %v", err)
	}
}
