//go:build multiproc

package l2

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// TestStorageFilesAcrossProcesses drives storage files (ADR 0053) through a
// real server and a real agent: every byte crosses the HTTP API, the server's
// data lane and the agent's filesystem, and the agent is killed in the middle
// of transfers to show a broken transfer never looks like a finished one.
//
//	go test -tags=multiproc -run 'TestStorageFiles' -count=1 -v ./test/l2/
func TestStorageFilesAcrossProcesses(t *testing.T) {
	probeRoot, outside := t.TempDir(), t.TempDir()
	root := filepath.Join(probeRoot, "store")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	h := newStreamHarnessWithConfig(t, serverFixtureConfig(t, serverconfig.File{StorageProbeRoots: []string{probeRoot}}))
	t.Cleanup(func() {
		if t.Failed() {
			for _, path := range []string{h.serverLog, h.agentLog} {
				t.Logf("%s:\n%s", path, readFileForLog(t, path))
			}
		}
	})
	sf := &storageFilesClient{t: t, h: h, root: root, http: &http.Client{Timeout: 2 * time.Minute}}
	body := sf.mustJSON(http.MethodPost, "/api/v1/storages", map[string]any{"name": "files-l2", "type": "local", "path": root, "enabled": true, "all_nodes": true}, http.StatusCreated, http.StatusOK)
	var created struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.ID == 0 {
		t.Fatalf("create storage: %s %v", body, err)
	}
	sf.id = created.ID
	sf.waitListable()

	t.Run("list root shows the virtual regions", func(t *testing.T) {
		page := sf.list("")
		if page.Writable {
			t.Fatalf("the root takes new entries: %+v", page)
		}
		names := page.names()
		for _, want := range []string{"files", ".gpuworks"} {
			item, ok := names[want]
			if !ok || !item.Dir || item.Writable {
				t.Fatalf("root entry %q = %+v (present %v), want a non-writable directory; page %+v", want, item, ok, page)
			}
		}
		if files := sf.list("files"); !files.Writable || len(files.Items) != 0 {
			t.Fatalf("files/ = %+v, want writable and empty", files)
		}
	})

	t.Run("upload with Content-Length lands at the target", func(t *testing.T) {
		content := []byte("hello storage files\n")
		res := sf.upload("files/a.txt", url.Values{}, bytes.NewReader(content), int64(len(content)))
		if res.Path != "files/a.txt" || res.Bytes != int64(len(content)) || res.Files != 1 {
			t.Fatalf("upload result = %+v", res)
		}
		item, ok := sf.list("files").names()["a.txt"]
		if !ok || item.Dir || item.Size != int64(len(content)) || !item.Writable || item.Path != "files/a.txt" {
			t.Fatalf("files/a.txt listed as %+v (present %v)", item, ok)
		}
		sf.assertFile("files/a.txt", content)
		sf.assertNoStaging()
	})

	t.Run("an existing target needs overwrite", func(t *testing.T) {
		sf.uploadRefused("files/a.txt", url.Values{}, []byte("second"), http.StatusConflict, "STORAGE_FILE_EXISTS")
		sf.assertFile("files/a.txt", []byte("hello storage files\n"))
		replaced := []byte("replaced content")
		sf.upload("files/a.txt", url.Values{"overwrite": {"true"}}, bytes.NewReader(replaced), int64(len(replaced)))
		sf.assertFile("files/a.txt", replaced)
		sf.assertNoStaging()
	})

	t.Run("archives unpack, escaping entries are refused", func(t *testing.T) {
		archive := tarGz(t, []tarEntry{{name: "top.txt", body: "top"}, {name: "sub/", dir: true}, {name: "sub/deep/inner.txt", body: "inner"}})
		res := sf.upload("files/unpacked", url.Values{"extract": {"tar.gz"}}, bytes.NewReader(archive), int64(len(archive)))
		if res.Path != "files/unpacked" || res.Files != 2 {
			t.Fatalf("extract result = %+v, want 2 files at files/unpacked", res)
		}
		sf.assertFile("files/unpacked/top.txt", []byte("top"))
		sf.assertFile("files/unpacked/sub/deep/inner.txt", []byte("inner"))

		evil := plainTar(t, []tarEntry{{name: "ok.txt", body: "fine"}, {name: "../evil", body: "escaped"}})
		sf.uploadRefused("files/evil-target", url.Values{"extract": {"tar"}}, evil, http.StatusUnprocessableEntity, "STORAGE_FILE_EXTRACT_FAILED")
		sf.assertMissing("files/evil-target")
		filepath.WalkDir(probeRoot, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.Name() == "evil" {
				t.Errorf("an escaping archive entry was written: %s", p)
			}
			return nil
		})
		sf.assertNoStaging()
	})

	t.Run("mkdir rename delete", func(t *testing.T) {
		if got := sf.mustJSON(http.MethodPost, sf.path("/files/mkdir"), map[string]any{"node_uuid": h.node.UUID, "path": "files/d/e"}, http.StatusOK); !bytes.Contains(got, []byte(`"files/d/e"`)) {
			t.Fatalf("mkdir answer %s", got)
		}
		if info, err := os.Stat(filepath.Join(root, "files/d/e")); err != nil || !info.IsDir() {
			t.Fatalf("mkdir left %v %v", info, err)
		}
		sf.upload("files/d/e/f.txt", url.Values{}, strings.NewReader("move me"), int64(len("move me")))
		sf.mustJSON(http.MethodPost, sf.path("/files/rename"), map[string]any{"node_uuid": h.node.UUID, "from": "files/d", "to": "files/d2"}, http.StatusOK)
		sf.assertMissing("files/d")
		sf.assertFile("files/d2/e/f.txt", []byte("move me"))
		sf.refused(http.MethodPost, sf.path("/files/rename"), mustJSON(t, map[string]any{"node_uuid": h.node.UUID, "from": "files/missing", "to": "files/x"}), http.StatusNotFound, "STORAGE_FILE_NOT_FOUND")

		resp, data := sf.do(http.MethodDelete, sf.path("/files")+"?"+url.Values{"node_uuid": {h.node.UUID}, "path": {"files/d2"}}.Encode(), nil, -1)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("delete: %d %s", resp.StatusCode, data)
		}
		sf.assertMissing("files/d2")
		if _, ok := sf.list("files").names()["d2"]; ok {
			t.Fatal("a deleted directory is still listed")
		}
		// The bytes go to the trash and are removed in the background.
		trash := filepath.Join(root, ".gpuworks", "trash")
		waitFor(t, 20*time.Second, "the trash to empty", func() bool {
			entries, err := os.ReadDir(trash)
			return err == nil && len(entries) == 0
		})
	})

	t.Run("writes outside the regions and bad paths are refused", func(t *testing.T) {
		sf.uploadRefused("Qwen/x", url.Values{}, []byte("x"), http.StatusForbidden, "STORAGE_FILE_REGION_READONLY")
		sf.refused(http.MethodPost, sf.path("/files/mkdir"), mustJSON(t, map[string]any{"node_uuid": h.node.UUID, "path": ".gpuworks/incoming/x"}), http.StatusForbidden, "STORAGE_FILE_REGION_READONLY")
		sf.refused(http.MethodDelete, sf.path("/files")+"?"+url.Values{"node_uuid": {h.node.UUID}, "path": {"files"}}.Encode(), nil, http.StatusForbidden, "STORAGE_FILE_REGION_READONLY")
		sf.uploadRefused("files/../x", url.Values{}, []byte("x"), http.StatusBadRequest, "STORAGE_FILE_PATH_INVALID")
		sf.refused(http.MethodGet, sf.path("/files")+"?path=files", nil, http.StatusBadRequest, "STORAGE_FILE_NODE_REQUIRED")
		for _, p := range []string{"Qwen", "x", ".gpuworks/incoming"} {
			if _, err := os.Lstat(filepath.Join(root, p, "x")); err == nil {
				t.Errorf("a refused write created %s/x", p)
			}
		}
	})

	t.Run("a link out of the storage is not followed", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("outside"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "files", "escape")); err != nil {
			t.Fatal(err)
		}
		item, ok := sf.list("files").names()["escape"]
		if !ok || !item.Symlink || item.Dir {
			t.Fatalf("files/escape listed as %+v (present %v), want a symlink", item, ok)
		}
		sf.uploadRefused("files/escape/x.txt", url.Values{}, []byte("x"), http.StatusBadRequest, "STORAGE_FILE_PATH_INVALID")
		sf.refused(http.MethodPost, sf.path("/files/mkdir"), mustJSON(t, map[string]any{"node_uuid": h.node.UUID, "path": "files/escape/sub"}), http.StatusBadRequest, "STORAGE_FILE_PATH_INVALID")
		entries, err := os.ReadDir(outside)
		if err != nil || len(entries) != 1 || entries[0].Name() != "keep.txt" {
			t.Fatalf("the directory outside the storage changed: %v %v", entries, err)
		}
		if err := os.Remove(filepath.Join(root, "files", "escape")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("download a file and a directory", func(t *testing.T) {
		resp, data := sf.do(http.MethodGet, sf.downloadPath("files/a.txt", ""), nil, -1)
		if resp.StatusCode != http.StatusOK || string(data) != "replaced content" || resp.ContentLength != int64(len("replaced content")) || resp.Header.Get("Content-Disposition") != `attachment; filename=a.txt` {
			t.Fatalf("file download: %d len=%d %q", resp.StatusCode, resp.ContentLength, data)
		}
		want := map[string]string{"unpacked/top.txt": "top", "unpacked/sub/deep/inner.txt": "inner"}
		resp, data = sf.do(http.MethodGet, sf.downloadPath("files/unpacked", ""), nil, -1)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/zip" || resp.Header.Get("Content-Disposition") != `attachment; filename=unpacked.zip` {
			t.Fatalf("zip download: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		assertArchive(t, "zip", unzipAll(t, data), want)
		resp, data = sf.do(http.MethodGet, sf.downloadPath("files/unpacked", "tar.gz"), nil, -1)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/gzip" || resp.Header.Get("Content-Disposition") != `attachment; filename=unpacked.tar.gz` {
			t.Fatalf("tar.gz download: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		assertArchive(t, "tar.gz", untarGzAll(t, data), want)
		sf.refused(http.MethodGet, sf.downloadPath("files/nope", ""), nil, http.StatusNotFound, "STORAGE_FILE_NOT_FOUND")
	})

	t.Run("upload without Content-Length", func(t *testing.T) {
		content := bytes.Repeat([]byte("chunked-"), 64<<10) // 512 KiB
		pr, pw := io.Pipe()
		go func() {
			for off := 0; off < len(content); off += 32 << 10 {
				if _, err := pw.Write(content[off : off+32<<10]); err != nil {
					return
				}
			}
			_ = pw.Close()
		}()
		res := sf.upload("files/chunked.bin", url.Values{}, pr, -1)
		if res.Bytes != int64(len(content)) {
			t.Fatalf("chunked upload result %+v, want %d bytes", res, len(content))
		}
		sf.assertFile("files/chunked.bin", content)
		sf.assertNoStaging()
	})

	t.Run("killing the agent breaks transfers in flight", func(t *testing.T) {
		sf.killMidTransfer()
	})

	t.Run("a disabled storage is read-only", func(t *testing.T) {
		sf.mustJSON(http.MethodPatch, sf.path(""), map[string]any{"enabled": false}, http.StatusOK)
		sf.uploadRefused("files/after-disable.txt", url.Values{}, []byte("x"), http.StatusConflict, "STORAGE_DISABLED")
		sf.refused(http.MethodPost, sf.path("/files/mkdir"), mustJSON(t, map[string]any{"node_uuid": h.node.UUID, "path": "files/nd"}), http.StatusConflict, "STORAGE_DISABLED")
		page := sf.list("files")
		if page.Writable {
			t.Fatalf("a disabled storage lists files/ as writable: %+v", page)
		}
		if item, ok := page.names()["a.txt"]; !ok || item.Writable {
			t.Fatalf("files/a.txt on a disabled storage = %+v (present %v)", item, ok)
		}
		resp, data := sf.do(http.MethodGet, sf.downloadPath("files/a.txt", ""), nil, -1)
		if resp.StatusCode != http.StatusOK || string(data) != "replaced content" {
			t.Fatalf("download from a disabled storage: %d %q", resp.StatusCode, data)
		}
		sf.assertMissing("files/after-disable.txt")
	})
}

// killMidTransfer starts an upload and two downloads (a file with a known
// length, a packed directory without one), kills the agent while all three
// are moving, and checks that none of them ends as a success; then a new
// agent process takes the same upload to completion.
func (sf *storageFilesClient) killMidTransfer() {
	t, h := sf.t, sf.h
	big := filepath.Join(sf.root, "files", "big.bin")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	const bigSize = 1 << 30 // sparse: no disk, more than any window holds
	if err := f.Truncate(bigSize); err != nil {
		t.Fatal(err)
	}
	f.Close()
	packed := filepath.Join(sf.root, "files", "packed")
	if err := os.MkdirAll(packed, 0o755); err != nil {
		t.Fatal(err)
	}
	noise := make([]byte, 64<<20) // incompressible, so the archive cannot finish early
	if _, err := rand.Read(noise); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packed, "noise.bin"), noise, 0o644); err != nil {
		t.Fatal(err)
	}

	// The upload: a body that sends 1 MiB and then trickles until the
	// transport gives up on it.
	pr, pw := io.Pipe()
	defer pw.Close()
	go func() {
		chunk := bytes.Repeat([]byte("u"), 64<<10)
		for i := 0; i < 16; i++ {
			if _, err := pw.Write(chunk); err != nil {
				return
			}
		}
		for i := 0; i < 600; i++ {
			time.Sleep(50 * time.Millisecond)
			if _, err := pw.Write(chunk[:1024]); err != nil {
				return
			}
		}
		_ = pw.CloseWithError(fmt.Errorf("trickle body gave up"))
	}()
	type outcome struct {
		status int
		body   string
		err    error
	}
	uploadDone := make(chan outcome, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPut, h.base+sf.uploadPath("files/killed.bin", url.Values{}), pr)
		req.Header.Set("Authorization", "Bearer "+h.adminToken)
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := sf.http.Do(req)
		if err != nil {
			uploadDone <- outcome{err: err}
			return
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		uploadDone <- outcome{status: resp.StatusCode, body: string(data), err: err}
	}()
	// The agent has the upload once its staging file holds bytes.
	waitFor(t, 30*time.Second, "the upload to reach the agent's staging", func() bool {
		select {
		case got := <-uploadDone:
			t.Fatalf("the trickled upload ended before the agent was killed: %+v", got)
		default:
		}
		matches, _ := filepath.Glob(filepath.Join(sf.root, ".gpuworks", "incoming", "*", "upload-*", "body"))
		for _, m := range matches {
			if info, err := os.Stat(m); err == nil && info.Size() > 0 {
				return true
			}
		}
		return false
	})

	openDownload := func(rel, format string) (*http.Response, int64) {
		req, _ := http.NewRequest(http.MethodGet, h.base+sf.downloadPath(rel, format), nil)
		req.Header.Set("Authorization", "Bearer "+h.adminToken)
		resp, err := sf.http.Do(req)
		if err != nil {
			t.Fatalf("download %s: %v", rel, err)
		}
		if resp.StatusCode != http.StatusOK {
			data, _ := io.ReadAll(resp.Body)
			t.Fatalf("download %s: %d %s", rel, resp.StatusCode, data)
		}
		n, err := io.CopyN(io.Discard, resp.Body, 64<<10)
		if err != nil {
			t.Fatalf("download %s: first bytes: %v", rel, err)
		}
		return resp, n
	}
	fileResp, fileRead := openDownload("files/big.bin", "")
	defer fileResp.Body.Close()
	if fileResp.ContentLength != bigSize {
		t.Fatalf("big.bin Content-Length = %d", fileResp.ContentLength)
	}
	dirResp, dirRead := openDownload("files/packed", "zip")
	defer dirResp.Body.Close()

	killProcess(t, h.agent)

	select {
	case got := <-uploadDone:
		if got.err == nil && got.status == http.StatusOK {
			t.Fatalf("an upload cut by a killed agent answered 200: %s", got.body)
		}
		t.Logf("upload after kill: status=%d err=%v body=%s", got.status, got.err, got.body)
	case <-time.After(60 * time.Second):
		t.Fatal("the upload did not end after the agent was killed")
	}
	sf.assertMissing("files/killed.bin")

	n, err := io.Copy(io.Discard, fileResp.Body)
	if err == nil || fileRead+n >= bigSize {
		t.Fatalf("file download after kill: read %d of %d, err %v; want a truncated, failed read", fileRead+n, int64(bigSize), err)
	}
	t.Logf("file download after kill: %d of %d bytes, err %v", fileRead+n, int64(bigSize), err)
	n, err = io.Copy(io.Discard, dirResp.Body)
	if err == nil {
		t.Fatalf("directory download after kill ended cleanly after %d bytes; a cut archive must fail the read", dirRead+n)
	}
	t.Logf("directory download after kill: %d bytes, err %v", dirRead+n, err)

	leftovers, _ := filepath.Glob(filepath.Join(sf.root, ".gpuworks", "incoming", "*", "upload-*"))
	t.Logf("staging left by the killed agent (swept after %s, not now): %v", "24h", leftovers)

	h.agent = startStreamAgent(t, h)
	waitNode(t, h.base, h.adminToken, func(node processNode) bool {
		return node.UUID == h.node.UUID && node.State == "online"
	})
	content := []byte("after the restart")
	waitFor(t, 30*time.Second, "an upload to the restarted agent", func() bool {
		resp, data := sf.do(http.MethodPut, sf.uploadPath("files/killed.bin", url.Values{}), bytes.NewReader(content), int64(len(content)))
		switch resp.StatusCode {
		case http.StatusOK:
			return true
		case http.StatusServiceUnavailable, http.StatusConflict:
			if strings.Contains(string(data), "STORAGE_FILE_EXISTS") {
				t.Fatalf("the killed upload left a target behind: %s", data)
			}
			return false // the lane or the storage is not back yet
		}
		t.Fatalf("upload after restart: %d %s", resp.StatusCode, data)
		return false
	})
	sf.assertFile("files/killed.bin", content)
}
