//go:build !linux || !dqlite

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A member keeps no gpuworks.db; the command must not go looking for one. A
// build without the cluster runtime cannot read a member's files, and says
// so instead.
func TestSupportBundleOnAClusterMemberDoesNotLookForASQLiteDatabase(t *testing.T) {
	_, data := memberLayout(t)
	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	code, stderr := runSupportBundle(t, "--data", data, "--out", out)
	if strings.Contains(stderr, "gpuworks.db") {
		t.Fatalf("the command looked for a sqlite database:\n%s", stderr)
	}
	if code == 0 || !strings.Contains(stderr, "this build carries no cluster runtime") || !strings.Contains(stderr, "the cluster's own gpuworks") {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	if entries, _ := os.ReadDir(filepath.Dir(out)); len(entries) != 0 {
		t.Fatalf("the failed collection left %d entries beside the output", len(entries))
	}
}
