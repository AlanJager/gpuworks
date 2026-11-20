//go:build linux && dqlite

package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

func TestClusterInitRequiresOfflineAndExplicitEndpoint(t *testing.T) {
	for _, args := range [][]string{{}, {"--offline"}, {"--offline", "--listen", "127.0.0.1:9443"}, {"--offline", "--public-url", "https://127.0.0.1:9443"}, {"--offline", "unexpected"}} {
		dir := t.TempDir()
		var output bytes.Buffer
		if err := clusterInitCommand(context.Background(), append([]string{"--data", dir}, args...), &output); err == nil {
			t.Fatalf("invalid invocation accepted: %v", args)
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("invalid invocation wrote state: %v %v", entries, err)
		}
	}
}

func TestClusterInitRefusesAnUnknownStorageTierBeforeWriting(t *testing.T) {
	for _, tier := range []string{"glacial", "100ms", "Standard"} {
		dir := t.TempDir()
		var output bytes.Buffer
		err := clusterInitCommand(context.Background(), []string{"--offline", "--data", dir, "--listen", "127.0.0.1:9443", "--public-url", "https://127.0.0.1:9443", "--storage-tier", tier}, &output)
		if err == nil || !strings.Contains(err.Error(), "--storage-tier: unknown storage tier") {
			t.Fatalf("--storage-tier %s: error %v", tier, err)
		}
		if entries, readErr := os.ReadDir(dir); readErr != nil || len(entries) != 0 {
			t.Fatalf("--storage-tier %s wrote state: %v %v", tier, entries, readErr)
		}
	}
}
