//go:build linux && dqlite

package cluster

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
)

func TestStorageTierArguments(t *testing.T) {
	if got := storageTierArguments(storagetier.Standard); got != nil {
		t.Fatalf("standard arguments = %v, want none so a process without the flag still starts", got)
	}
	if got := storageTierArguments(storagetier.SlowStorage); !reflect.DeepEqual(got, []string{"--network-latency-ms", "50"}) {
		t.Fatalf("slow-storage arguments = %v", got)
	}
}

// The database process is started with the tier's latency: from the
// configuration when it names one, else from the process's installation.
func TestStartDatabasePassesTheStorageTierToTheProcess(t *testing.T) {
	previous := processStorageTier.Load()
	t.Cleanup(func() { processStorageTier.Store(previous) })
	for _, tc := range []struct {
		name       string
		process    storagetier.Tier
		configured storagetier.Tier
		want       string
	}{
		{"standard", "", "", ""},
		{"process slow-storage", storagetier.SlowStorage, "", "--network-latency-ms 50"},
		{"configured slow-storage", "", storagetier.SlowStorage, "--network-latency-ms 50"},
		{"configured standard over process", storagetier.SlowStorage, storagetier.Standard, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			processStorageTier.Store(nil)
			if tc.process != "" {
				UseStorageTier(tc.process)
			}
			recorded := filepath.Join(t.TempDir(), "argv")
			// The script records its arguments and then fails, so no SQL pool
			// is opened against a process that is not a database.
			fakeHelper(t, `printf '%s\n' "$*" > `+recorded+`; echo refused >&2; exit 1`)
			_, err := StartDatabase(t.Context(), DatabaseConfig{Dir: filepath.Join(t.TempDir(), "raft"), Name: "db", Address: "127.0.0.1:9001", StorageTier: tc.configured})
			if err == nil {
				t.Fatal("a refusing process started the database")
			}
			raw, readErr := os.ReadFile(recorded)
			if readErr != nil {
				t.Fatalf("process was not started: %v (start error %v)", readErr, err)
			}
			argv := strings.TrimSpace(string(raw))
			if tc.want == "" && strings.Contains(argv, "--network-latency-ms") {
				t.Fatalf("argv %q names a latency for the standard tier", argv)
			}
			if tc.want != "" && !strings.HasSuffix(argv, tc.want) {
				t.Fatalf("argv %q, want it to end with %q", argv, tc.want)
			}
		})
	}
}
