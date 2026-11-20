//go:build linux && dqlite

package cluster

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

// requireDatabaseHelper skips a test that needs the real database process when
// none is configured. Strict runs treat the missing binary as a failure.
func requireDatabaseHelper(t testing.TB) {
	t.Helper()
	path := os.Getenv(helperBinaryEnv)
	if info, err := os.Stat(path); path != "" && err == nil && info.Mode().IsRegular() {
		return
	}
	if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
		t.Fatalf("%s must name the %s executable", helperBinaryEnv, helperBinaryName)
	}
	t.Skipf("%s does not name the %s executable", helperBinaryEnv, helperBinaryName)
}

// lastRaftEntryIndex reads the last entry of a stopped raft directory through
// the database process.
func lastRaftEntryIndex(t *testing.T, ctx context.Context, dir string) uint64 {
	t.Helper()
	out, err := runHelper(ctx, "describe-last", "--dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	var info struct {
		Index uint64 `json:"index"`
	}
	if err := json.Unmarshal(out, &info); err != nil || info.Index == 0 {
		t.Fatalf("describe-last did not report the last entry index: %q %v", out, err)
	}
	return info.Index
}
