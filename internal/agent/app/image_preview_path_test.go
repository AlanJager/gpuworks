package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// Pausing at a compressed byte offset lets the deadline expire after tar
// scanning started, regardless of the machine's disk or decompression speed.
type delayedArchiveRead struct {
	io.ReadSeeker
	offset int64
	paused bool
}

func (r *delayedArchiveRead) Read(p []byte) (int, error) {
	if !r.paused {
		pos, err := r.Seek(0, io.SeekCurrent)
		if err != nil {
			return 0, err
		}
		left := r.offset - pos
		if left == 0 {
			r.paused = true
			time.Sleep(proto.ImageArchivePreviewTimeout)
			synctest.Wait()
		} else if left < int64(len(p)) {
			p = p[:left]
		}
	}
	return r.ReadSeeker.Read(p)
}

func TestNodeArchivePreviewDeadlineDuringGzipScan(t *testing.T) {
	for _, tc := range []struct {
		name        string
		files, size int
	}{{"many small headers", 128, 8}, {"expanded layer", 1, 256 << 10}} {
		t.Run(tc.name, func(t *testing.T) {
			var archive bytes.Buffer
			tw := tar.NewWriter(&archive)
			writeEntry := func(name string, data []byte) {
				t.Helper()
				if err := tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(data)), Mode: 0600}); err != nil {
					t.Fatal(err)
				}
				if _, err := tw.Write(data); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < tc.files; i++ {
				writeEntry(fmt.Sprintf("layers/%d", i), bytes.Repeat([]byte{'a'}, tc.size))
			}
			writeEntry("manifest.json", []byte(`[{"RepoTags":["example/image:v1"]}]`))
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			var compressed bytes.Buffer
			gz := gzip.NewWriter(&compressed)
			mid := archive.Len() / 2
			if _, err := gz.Write(archive.Bytes()[:mid]); err != nil {
				t.Fatal(err)
			}
			if err := gz.Flush(); err != nil {
				t.Fatal(err)
			}
			offset := int64(compressed.Len())
			if _, err := gz.Write(archive.Bytes()[mid:]); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}

			path := filepath.Join(t.TempDir(), "image.tar.gz")
			if err := os.WriteFile(path, compressed.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			driver := recording.New(filepath.Join(t.TempDir(), "spec.json"))
			got, err := previewPath(t, driver, path)
			if err != nil || len(got.(proto.ImagePreviewPathResult).RepoTags) != 1 {
				t.Fatalf("complete preview = %+v, %v", got, err)
			}
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), proto.ImageArchivePreviewTimeout)
				defer cancel()
				reader := &delayedArchiveRead{ReadSeeker: bytes.NewReader(compressed.Bytes()), offset: offset}
				got, err := readNodeArchivePreview(ctx, reader)
				expectArchivePreviewReason(t, err, proto.ArchivePreviewTimeout)
				if !reader.paused || got != nil {
					t.Fatal("expected the scan to stop without a partial preview")
				}
			})
		})
	}
}
