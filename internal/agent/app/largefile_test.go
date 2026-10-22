package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestStreamLargeFileServesOneRegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokenizer.json")
	content := strings.Repeat(`{"model":{"vocab":{}}}`, 1024)
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := (&NodeRuntime{}).StreamLargeFile(context.Background(), path, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != content {
		t.Fatalf("streamed %d bytes, want %d", out.Len(), len(content))
	}
}

func TestStreamLargeFileRefusesWhatItCannotServeIntact(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "tokenizer.json")
	if err := os.WriteFile(file, []byte(`{"model":{}}`), 0o640); err != nil {
		t.Fatal(err)
	}
	oversize := filepath.Join(dir, "oversize.json")
	if err := os.WriteFile(oversize, make([]byte, proto.LargeFileReadMaxBytes+1), 0o640); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		path string
	}{
		{name: "a relative path", path: filepath.Join(filepath.Base(dir), "tokenizer.json")},
		{name: "a directory", path: dir},
		{name: "a missing file", path: filepath.Join(dir, "absent.json")},
		{name: "an oversized file", path: oversize},
		{name: "an empty path", path: "  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := (&NodeRuntime{}).StreamLargeFile(context.Background(), tc.path, &out); err == nil {
				t.Fatalf("path %q streamed %d bytes, want an error", tc.path, out.Len())
			}
			if out.Len() != 0 {
				t.Fatalf("a refused read still emitted %d bytes", out.Len())
			}
		})
	}
}

func TestStreamLargeFileStopsOnCancellation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokenizer.json")
	if err := os.WriteFile(path, make([]byte, 4096), 0o640); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (&NodeRuntime{}).StreamLargeFile(ctx, path, &bytes.Buffer{}); err == nil {
		t.Fatal("a cancelled read must not report success")
	}
}

// fs.stat is what lets the server adopt a node-local file without pulling its
// bytes, so it has to tell a file from a directory and from nothing at all.
func TestRPCFSStatReportsFileType(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "tokenizer.json")
	if err := os.WriteFile(file, []byte("0123456789"), 0o640); err != nil {
		t.Fatal(err)
	}
	n := &NodeRuntime{}

	raw, err := n.rpcFSStat(json.RawMessage(`{"path":"` + file + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := raw.(proto.FSStatResult)
	if !ok {
		t.Fatalf("result type = %T", raw)
	}
	if stat.Path != file || stat.SizeBytes != 10 || !stat.Regular || stat.Dir {
		t.Fatalf("stat = %+v", stat)
	}

	raw, err = n.rpcFSStat(json.RawMessage(`{"path":"` + dir + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	if stat = raw.(proto.FSStatResult); stat.Regular || !stat.Dir {
		t.Fatalf("directory stat = %+v", stat)
	}

	if _, err := n.rpcFSStat(json.RawMessage(`{"path":"` + filepath.Join(dir, "absent") + `"}`)); err == nil {
		t.Fatal("a missing path must be an error, not an empty answer")
	}
	if _, err := n.rpcFSStat(json.RawMessage(`{"path":"  "}`)); err == nil {
		t.Fatal("an empty path must be refused")
	}
}
