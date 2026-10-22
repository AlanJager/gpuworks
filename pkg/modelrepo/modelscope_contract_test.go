package modelrepo

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// This is an explicit external contract sentinel. Default tests exercise the
// same request/response behavior through local fixtures; public-host drift is
// checked only in a network-enabled low-frequency lane.
func TestModelScopePublicListFilesContract(t *testing.T) {
	if os.Getenv("GPUWORKS_TEST_NET") != "1" {
		t.Skip("set GPUWORKS_TEST_NET=1 to validate the public ModelScope API")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	files, err := ListFiles(ctx, SourceMS, "zstack/qwen3-4b-fake", "", "", nil)
	if err != nil {
		t.Fatalf("ModelScope public ListFiles contract failed (platform API drift or CI runner network): %v", err)
	}
	if len(files) == 0 {
		t.Fatal("ModelScope public ListFiles contract failed: platform returned an empty repository manifest")
	}
	for _, file := range files {
		if strings.TrimSpace(file.Revision) != "" {
			return
		}
	}
	t.Fatalf("ModelScope public ListFiles contract failed: %d files returned without a commit revision", len(files))
}
