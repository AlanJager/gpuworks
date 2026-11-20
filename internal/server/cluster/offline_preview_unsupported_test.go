//go:build !linux || !dqlite

package cluster

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOfflineSingleExportUnavailableDoesNotCreateOutput(t *testing.T) {
	parent := t.TempDir()
	native, output := filepath.Join(parent, "native"), filepath.Join(parent, "output")
	err := ExportOfflineSingleDatabase(t.Context(), native, "gpuworks.db", Member{ID: 1, Address: "127.0.0.1:19000", Role: "voter"}, output)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("native reader unavailable error: %v", err)
	}
	for _, path := range []string{native, output} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unavailable native export changed path %s: %v", path, err)
		}
	}
}
