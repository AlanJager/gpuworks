package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/sys/unix"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/test/ocifixture"
)

func previewPath(t *testing.T, driver runtime.Driver, path string) (any, error) {
	t.Helper()
	node := NewNodeRuntime(slog.Default(), driver, t.TempDir())
	raw, err := json.Marshal(proto.ImagePreviewPathParams{ArchivePath: path})
	if err != nil {
		t.Fatal(err)
	}
	return node.OnRPC(t.Context(), proto.RPCImagePreviewPath, raw)
}

func expectArchivePreviewReason(t *testing.T, err error, reason string) {
	t.Helper()
	var rpc *proto.RPCError
	if !errors.As(err, &rpc) || rpc.Code != proto.ErrorArchivePreview || rpc.Params["reason"] != reason {
		t.Fatalf("preview error = %#v, want %s", err, reason)
	}
}

func TestNodeArchivePreviewUsesRegularFileAndResolvesGzipThroughLink(t *testing.T) {
	dir := t.TempDir()
	const ref = "example/runner:v1"
	data, err := ocifixture.DockerSaveArchive(ocifixture.DockerSaveOptions{RepoTags: []string{ref}, Labels: map[string]string{"io.gpuworks.engine": "vllm"}})
	if err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "image.gz"), gz.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.tar")
	if err := os.Symlink("image.gz", link); err != nil {
		t.Fatal(err)
	}
	driver := recording.New(filepath.Join(dir, "spec.json"))
	got, err := previewPath(t, driver, link)
	if err != nil {
		t.Fatal(err)
	}
	result := got.(proto.ImagePreviewPathResult)
	if len(result.RepoTags) != 1 || result.RepoTags[0] != ref || result.MetadataByTag[ref].Engine != "vllm" {
		t.Fatalf("preview = %+v", result)
	}
	fifo := filepath.Join(dir, "pipe")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "truncated.tar")
	if err := os.WriteFile(bad, data[:521], 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, reason string }{{dir, proto.ArchivePreviewNotRegular}, {fifo, proto.ArchivePreviewNotRegular}, {"/dev/null", proto.ArchivePreviewNotRegular}, {bad, proto.ArchivePreviewInvalid}, {filepath.Join(dir, "missing"), proto.ArchivePreviewNotFound}} {
		_, err := previewPath(t, driver, tc.path)
		expectArchivePreviewReason(t, err, tc.reason)
	}
}

type archiveOpenFailure struct {
	runtime.Driver
	open func(context.Context) (*os.File, error)
}

func (d archiveOpenFailure) OpenImageFile(ctx context.Context, _ string) (*os.File, error) {
	return d.open(ctx)
}

func TestNodeArchivePreviewPermissionAndDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base := recording.New(filepath.Join(t.TempDir(), "spec.json"))
		driver := archiveOpenFailure{base, func(context.Context) (*os.File, error) { return nil, os.ErrPermission }}
		_, err := previewPath(t, driver, "/data/runner.tar")
		expectArchivePreviewReason(t, err, proto.ArchivePreviewPermission)
		driver.open = func(ctx context.Context) (*os.File, error) { <-ctx.Done(); return nil, ctx.Err() }
		start := time.Now()
		_, err = previewPath(t, driver, "/data/runner.tar")
		expectArchivePreviewReason(t, err, proto.ArchivePreviewTimeout)
		if elapsed := time.Since(start); elapsed != proto.ImageArchivePreviewTimeout {
			t.Fatalf("scan duration = %s", elapsed)
		}
	})
}
