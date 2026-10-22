//go:build linux && l4

package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/sys/unix"

	"gpuworks.ai/gpuworks/internal/proto"
)

// TestL4StorageModelCacheCopyNFS is l4-storage-model-cache-copy-nfs. The
// caller supplies a small read-only NFS directory (or local symlinks to NFS
// files); this case never mounts, writes to, or disrupts the source server.
func TestL4StorageModelCacheCopyNFS(t *testing.T) {
	source := os.Getenv("GPUWORKS_NFS_COPY_SOURCE")
	if source == "" {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal("GPUWORKS_NFS_COPY_SOURCE is required")
		}
		t.Skip("explicit read-only NFS fixture required")
	}
	files, err := listCopySource(source)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	nfs := false
	for _, file := range files {
		total += file.size
		path, err := filepath.EvalSymlinks(filepath.Join(source, file.rel))
		if err != nil {
			t.Fatal(err)
		}
		var stat unix.Statfs_t
		if err := unix.Statfs(path, &stat); err != nil {
			t.Fatal(err)
		}
		nfs = nfs || uint64(stat.Type) == 0x6969
	}
	if !nfs || total == 0 || total > 16<<20 {
		t.Fatalf("requires a nonempty NFS fixture <=16 MiB; nfs=%v bytes=%d", nfs, total)
	}
	target := filepath.Join(t.TempDir(), "cache")
	n := newNodeRuntime(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, filepath.Dir(target), "none", metricsOnlyProber{}, nil)
	var mu sync.Mutex
	var last proto.ModelFileStatus
	n.Bind(func(kind string, value any) error {
		if kind == proto.TStatusModelFile {
			mu.Lock()
			last = value.(proto.ModelFileStatus)
			mu.Unlock()
		}
		return nil
	})
	task := copyTask("nfs-readonly", source)
	task.RateLimitKBs = 1024
	task.TransferID = "readonly-nfs"
	n.copyModelDir(context.Background(), task, target, target)
	mu.Lock()
	result := last
	mu.Unlock()
	if result.State != proto.MFPresent {
		t.Fatalf("copy=%+v", result)
	}
	for _, file := range files {
		hash := func(path string) string {
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			h := sha256.New()
			if _, err := io.Copy(h, f); err != nil {
				t.Fatal(err)
			}
			return fmt.Sprintf("%x", h.Sum(nil))
		}
		if hash(filepath.Join(source, file.rel)) != hash(filepath.Join(target, file.rel)) {
			t.Fatalf("contents differ: %s", file.rel)
		}
	}
	t.Logf("read-only NFS copy verified: files=%d bytes=%d; source was not modified", len(files), total)
}
