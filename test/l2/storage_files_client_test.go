//go:build multiproc

package l2

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
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
)

// storageFilesClient is the test's view of one storage on one node.
type storageFilesClient struct {
	t    *testing.T
	h    *streamHarness
	http *http.Client
	id   uint
	root string
}

type storageFileItem struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Dir      bool   `json:"dir"`
	Symlink  bool   `json:"symlink"`
	Size     int64  `json:"size"`
	Writable bool   `json:"writable"`
}

type storageFileList struct {
	Path       string            `json:"path"`
	NodeUUID   string            `json:"node_uuid"`
	Writable   bool              `json:"writable"`
	Items      []storageFileItem `json:"items"`
	NextCursor string            `json:"next_cursor"`
}

func (l storageFileList) names() map[string]storageFileItem {
	out := map[string]storageFileItem{}
	for _, item := range l.Items {
		out[item.Name] = item
	}
	return out
}

type storageUploadResult struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
	Files int    `json:"files"`
}

func (sf *storageFilesClient) path(suffix string) string {
	return fmt.Sprintf("/api/v1/storages/%d%s", sf.id, suffix)
}

func (sf *storageFilesClient) uploadPath(rel string, q url.Values) string {
	q.Set("node_uuid", sf.h.node.UUID)
	q.Set("path", rel)
	return sf.path("/files/upload") + "?" + q.Encode()
}

func (sf *storageFilesClient) downloadPath(rel, format string) string {
	q := url.Values{"node_uuid": {sf.h.node.UUID}, "path": {rel}}
	if format != "" {
		q.Set("format", format)
	}
	return sf.path("/files/download") + "?" + q.Encode()
}

// do sends one request; size is the body's Content-Length (-1: none, so a
// non-nil body goes chunked).
func (sf *storageFilesClient) do(method, path string, body io.Reader, size int64) (*http.Response, []byte) {
	sf.t.Helper()
	req, err := http.NewRequest(method, sf.h.base+path, body)
	if err != nil {
		sf.t.Fatal(err)
	}
	if body != nil {
		req.ContentLength = size
		if size < 0 {
			req.ContentLength = 0 // unknown length with a body: chunked
			req.Body = io.NopCloser(body)
		}
	}
	req.Header.Set("Authorization", "Bearer "+sf.h.adminToken)
	if method == http.MethodPut {
		req.Header.Set("Content-Type", "application/octet-stream")
	} else {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := sf.http.Do(req)
	if err != nil {
		sf.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		sf.t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return resp, data
}

func (sf *storageFilesClient) mustJSON(method, path string, value any, want ...int) []byte {
	sf.t.Helper()
	body := mustJSON(sf.t, value)
	resp, data := sf.do(method, path, bytes.NewReader(body), int64(len(body)))
	for _, status := range want {
		if resp.StatusCode == status {
			return data
		}
	}
	sf.t.Fatalf("%s %s: %d %s, want %v", method, path, resp.StatusCode, data, want)
	return nil
}

// refused checks a request fails with status and the product error code.
func (sf *storageFilesClient) refused(method, path string, body []byte, status int, code string) {
	sf.t.Helper()
	var reader io.Reader
	size := int64(-1)
	if body != nil {
		reader, size = bytes.NewReader(body), int64(len(body))
	}
	resp, data := sf.do(method, path, reader, size)
	var answer struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(data, &answer)
	if resp.StatusCode != status || answer.Code != code {
		sf.t.Fatalf("%s %s: %d %s, want %d %s", method, path, resp.StatusCode, data, status, code)
	}
}

func (sf *storageFilesClient) list(rel string) storageFileList {
	sf.t.Helper()
	q := url.Values{"node_uuid": {sf.h.node.UUID}, "path": {rel}}
	resp, data := sf.do(http.MethodGet, sf.path("/files")+"?"+q.Encode(), nil, -1)
	if resp.StatusCode != http.StatusOK {
		sf.t.Fatalf("list %q: %d %s", rel, resp.StatusCode, data)
	}
	var page storageFileList
	if err := json.Unmarshal(data, &page); err != nil {
		sf.t.Fatalf("list %q: %v %s", rel, err, data)
	}
	if page.Path != rel || page.NodeUUID != sf.h.node.UUID {
		sf.t.Fatalf("list %q answered for %q on %q", rel, page.Path, page.NodeUUID)
	}
	return page
}

// waitListable waits for the agent to report the new storage ready.
func (sf *storageFilesClient) waitListable() {
	sf.t.Helper()
	q := url.Values{"node_uuid": {sf.h.node.UUID}, "path": {""}}
	var last string
	waitFor(sf.t, 30*time.Second, "the storage to become browsable", func() bool {
		resp, data := sf.do(http.MethodGet, sf.path("/files")+"?"+q.Encode(), nil, -1)
		last = fmt.Sprintf("%d %s", resp.StatusCode, data)
		return resp.StatusCode == http.StatusOK
	})
	sf.t.Logf("storage browsable: %s", last)
}

func (sf *storageFilesClient) upload(rel string, q url.Values, body io.Reader, size int64) storageUploadResult {
	sf.t.Helper()
	resp, data := sf.do(http.MethodPut, sf.uploadPath(rel, q), body, size)
	if resp.StatusCode != http.StatusOK {
		sf.t.Fatalf("upload %s: %d %s", rel, resp.StatusCode, data)
	}
	var res storageUploadResult
	if err := json.Unmarshal(data, &res); err != nil {
		sf.t.Fatalf("upload %s: %v %s", rel, err, data)
	}
	return res
}

func (sf *storageFilesClient) uploadRefused(rel string, q url.Values, body []byte, status int, code string) {
	sf.t.Helper()
	sf.refused(http.MethodPut, sf.uploadPath(rel, q), body, status, code)
}

func (sf *storageFilesClient) assertFile(rel string, want []byte) {
	sf.t.Helper()
	got, err := os.ReadFile(filepath.Join(sf.root, filepath.FromSlash(rel)))
	if err != nil || !bytes.Equal(got, want) {
		sf.t.Fatalf("%s on the agent's disk = %d bytes (%v), want %d bytes", rel, len(got), err, len(want))
	}
}

func (sf *storageFilesClient) assertMissing(rel string) {
	sf.t.Helper()
	if _, err := os.Lstat(filepath.Join(sf.root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
		sf.t.Fatalf("%s exists on the agent's disk (err %v)", rel, err)
	}
}

// assertNoStaging checks no upload staging is left in the storage.
func (sf *storageFilesClient) assertNoStaging() {
	sf.t.Helper()
	matches, _ := filepath.Glob(filepath.Join(sf.root, ".gpuworks", "incoming", "*", "upload-*"))
	if len(matches) != 0 {
		sf.t.Fatalf("upload staging left behind: %v", matches)
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if ok() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

type tarEntry struct {
	name, body string
	dir        bool
}

func writeTar(t *testing.T, w io.Writer, entries []tarEntry) {
	t.Helper()
	tw := tar.NewWriter(w)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg, ModTime: time.Now()}
		if e.dir {
			hdr.Typeflag, hdr.Mode, hdr.Size = tar.TypeDir, 0o755, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
}

func plainTar(t *testing.T, entries []tarEntry) []byte {
	var buf bytes.Buffer
	writeTar(t, &buf, entries)
	return buf.Bytes()
}

func tarGz(t *testing.T, entries []tarEntry) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	writeTar(t, gz, entries)
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// unzipAll returns the regular files of a zip by name.
func unzipAll(t *testing.T, data []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("zip entry %s: %v", f.Name, err)
		}
		out[f.Name] = string(body)
	}
	return out
}

// untarGzAll returns the regular files of a tar.gz by name.
func untarGzAll(t *testing.T, data []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("read gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[hdr.Name] = string(body)
	}
}

func assertArchive(t *testing.T, kind string, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s holds %v, want %v", kind, got, want)
	}
	for name, body := range want {
		if got[name] != body {
			t.Fatalf("%s entry %s = %q, want %q (all: %v)", kind, name, got[name], body, got)
		}
	}
}
