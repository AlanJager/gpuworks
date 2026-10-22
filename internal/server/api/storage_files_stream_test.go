package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// An upload streams the body to the agent with the target in the header, and
// answers with what the agent reported.
func TestUploadStorageFileStreamsAndAnswers(t *testing.T) {
	rig := newStorageFilesRig(t)
	rec := rig.do(rig.admin, http.MethodPut, rig.url(rig.shared, "/upload?path=files/a.txt&overwrite=true"), strings.NewReader("hello world!"), "Content-Type", "application/octet-stream")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"path":"files/a.txt","bytes":12,"files":1}` {
		t.Fatalf("upload = %d %s", rec.Code, rec.Body)
	}
	node, hdr := rig.streams.lastHeader(t)
	staging := regexp.MustCompile(`^\.gpuworks/incoming/` + regexp.QuoteMeta(rig.st.ControlPlaneIdentity().ID) + `/upload-[0-9a-f]{32}$`)
	if node != rig.n1.UUID || hdr.StorageID != rig.shared.ID || hdr.Rel != "files/a.txt" || hdr.Size != 12 || hdr.Extract != "" || !hdr.Overwrite || !staging.MatchString(hdr.Staging) {
		t.Fatalf("node %s header = %+v", node, hdr)
	}

	rec = rig.do(rig.admin, http.MethodPut, rig.url(rig.local, "/upload?node_uuid="+rig.n2.UUID+"&path=.gpuworks/apps/eval/data&extract=tar.gz"), strings.NewReader("archive"))
	if rec.Code != http.StatusOK {
		t.Fatalf("extract upload = %d %s", rec.Code, rec.Body)
	}
	node, second := rig.streams.lastHeader(t)
	if node != rig.n2.UUID || second.Extract != proto.StorageArchiveTarGz || second.Overwrite || second.Rel != ".gpuworks/apps/eval/data" || second.Staging == hdr.Staging {
		t.Fatalf("node %s header = %+v", node, second)
	}
}

// Without a Content-Length the size is unknown (-1) and the body still
// streams; the handler hands the agent each chunk as it arrives instead of
// collecting the body, and writes no temporary file.
func TestUploadStorageFileStreamsWithoutBuffering(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	rig := newStorageFilesRig(t)
	firstChunk := make(chan struct{})
	rig.streams.upload = func(_ context.Context, _ string, hdr proto.StreamHeader, body io.Reader) (proto.StorageFilesStatus, error) {
		buf := make([]byte, 5)
		if _, err := io.ReadFull(body, buf); err != nil || string(buf) != "first" {
			return proto.StorageFilesStatus{}, errors.New("first chunk missing")
		}
		close(firstChunk) // the client has not sent the rest yet
		n, err := io.Copy(io.Discard, body)
		return proto.StorageFilesStatus{State: proto.StorageFilesDone, Bytes: n + 5, Files: 1}, err
	}
	pr, pw := io.Pipe()
	const total = 64 << 20
	go func() {
		_, _ = pw.Write([]byte("first"))
		select {
		case <-firstChunk:
		case <-time.After(5 * time.Second):
			_ = pw.CloseWithError(errors.New("the agent never saw the first chunk before the body ended"))
			return
		}
		chunk := bytes.Repeat([]byte("z"), 1<<20)
		for sent := 0; sent < total; sent += len(chunk) {
			if _, err := pw.Write(chunk); err != nil {
				return
			}
		}
		_ = pw.Close()
	}()
	req := httptest.NewRequest(http.MethodPut, rig.url(rig.shared, "/upload?path=files/big"), pr)
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+rig.admin)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	rec := httptest.NewRecorder()
	rig.h.ServeHTTP(rec, req)
	runtime.ReadMemStats(&after)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"bytes":67108869`) {
		t.Fatalf("upload = %d %s", rec.Code, rec.Body)
	}
	if _, hdr := rig.streams.lastHeader(t); hdr.Size != -1 {
		t.Fatalf("size = %d, want -1 without a Content-Length", hdr.Size)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > total/4 {
		t.Fatalf("handling a %d-byte upload allocated %d bytes: the body was buffered", total, grew)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Fatalf("upload left temporary files: %v", entries)
	}
}

// Every check that does not need the agent answers before the body is read,
// so the browser gets the JSON error rather than a reset upload.
func TestUploadStorageFilePrechecksBeforeReadingTheBody(t *testing.T) {
	rig := newStorageFilesRig(t)
	put := func(target string, status int, code string) {
		t.Helper()
		body := &touchedBody{Reader: strings.NewReader("payload")}
		expectError(t, rig.do(rig.admin, http.MethodPut, target, body), status, code)
		if body.wasRead() {
			t.Fatalf("%s: body read before refusing with %s", target, code)
		}
	}
	put(rig.url(rig.shared, "/upload?path=models/x"), http.StatusForbidden, ErrorStorageFileRegionReadonly)
	put(rig.url(rig.shared, "/upload?path=files"), http.StatusForbidden, ErrorStorageFileRegionReadonly)
	put(rig.url(rig.shared, "/upload"), http.StatusForbidden, ErrorStorageFileRegionReadonly)
	put(rig.url(rig.shared, "/upload?path=files/../../etc"), http.StatusBadRequest, ErrorStorageFilePathInvalid)
	put(rig.url(rig.shared, "/upload?path=files/a&extract=rar"), http.StatusBadRequest, ErrorFieldInvalid)
	put(rig.url(rig.shared, "/upload?path=files/a&overwrite=maybe"), http.StatusBadRequest, ErrorFieldInvalid)
	put(rig.url(rig.local, "/upload?path=files/a"), http.StatusBadRequest, ErrorStorageFileNodeRequired)

	rig.streams.setAttached(rig.n1.UUID, false)
	put(rig.url(rig.local, "/upload?node_uuid="+rig.n1.UUID+"&path=files/a"), http.StatusServiceUnavailable, ErrorStorageFileDataLaneUnavailable)
	rig.streams.setAttached(rig.n1.UUID, true)
	rig.setCaps(rig.n1.UUID)
	put(rig.url(rig.local, "/upload?node_uuid="+rig.n1.UUID+"&path=files/a"), http.StatusConflict, ErrorStorageFileAgentUnsupported)
	rig.setCaps(rig.n1.UUID, proto.CapStorageFiles)
	rig.setOnline(rig.n1.UUID, false)
	put(rig.url(rig.local, "/upload?node_uuid="+rig.n1.UUID+"&path=files/a"), http.StatusConflict, ErrorNodeOffline)
	rig.setOnline(rig.n1.UUID, true)
	rig.setStorageState(rig.n1, true, false)
	put(rig.url(rig.local, "/upload?node_uuid="+rig.n1.UUID+"&path=files/a"), http.StatusConflict, ErrorStorageNotWritable)
	rig.disable()
	put(rig.url(rig.shared, "/upload?path=files/a"), http.StatusConflict, ErrorStorageDisabled)
	if rig.streams.opened() != 0 {
		t.Fatal("a refused upload opened a stream")
	}
}

// The agent's verdict (a refusal mid-way, a failed extraction) keeps its
// code; a broken transfer is an operation failure unless the lane went away.
func TestUploadStorageFileAgentFailures(t *testing.T) {
	rig := newStorageFilesRig(t)
	for _, tc := range []struct {
		status proto.StorageFilesStatus
		err    error
		lane   bool
		want   int
		code   string
	}{
		{status: proto.StorageFilesStatus{State: proto.StorageFilesFailed, Code: proto.StorageFilesErrExtractFailed, Error: "entry escapes"}, lane: true, want: http.StatusUnprocessableEntity, code: ErrorStorageFileExtractFailed},
		{status: proto.StorageFilesStatus{State: proto.StorageFilesFailed, Code: proto.StorageFilesErrExists, Error: "files/a"}, lane: true, want: http.StatusConflict, code: ErrorStorageFileExists},
		{status: proto.StorageFilesStatus{State: proto.StorageFilesFailed, Code: proto.StorageFilesErrNotWritable, Error: "no space"}, lane: true, want: http.StatusConflict, code: ErrorStorageNotWritable},
		{status: proto.StorageFilesStatus{State: proto.StorageFilesFailed, Error: "?"}, lane: true, want: http.StatusBadGateway, code: ErrorStorageFileOperationFailed},
		{err: errors.New("stream reset"), lane: true, want: http.StatusBadGateway, code: ErrorStorageFileOperationFailed},
		{err: errors.New("data lane closed"), lane: false, want: http.StatusServiceUnavailable, code: ErrorStorageFileDataLaneUnavailable},
	} {
		rig.streams.upload = func(_ context.Context, uuid string, _ proto.StreamHeader, body io.Reader) (proto.StorageFilesStatus, error) {
			_, _ = io.CopyN(io.Discard, body, 3)
			rig.streams.setAttached(uuid, tc.lane)
			return tc.status, tc.err
		}
		expectError(t, rig.do(rig.admin, http.MethodPut, rig.url(rig.shared, "/upload?path=files/a"), strings.NewReader("payload")), tc.want, tc.code)
		rig.streams.setAttached(rig.n1.UUID, true)
		rig.streams.setAttached(rig.n2.UUID, true)
	}
}

// A client that goes away mid-upload cancels the transfer: the stream sees
// the request's context end and the broken body.
func TestUploadStorageFileClientDisconnectCancels(t *testing.T) {
	rig := newStorageFilesRig(t)
	observed := make(chan error, 1)
	started := make(chan struct{})
	rig.streams.upload = func(ctx context.Context, _ string, _ proto.StreamHeader, body io.Reader) (proto.StorageFilesStatus, error) {
		close(started)
		_, err := io.Copy(io.Discard, body)
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			err = errors.New("request context still live after the client left")
		}
		observed <- err
		return proto.StorageFilesStatus{}, err
	}
	srv := httptest.NewServer(rig.h)
	defer srv.Close()
	// A raw connection, so the test controls exactly when the client leaves:
	// a promised body, its first bytes, then the connection closes.
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	request := "PUT " + rig.url(rig.shared, "/upload?path=files/a") + " HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer " + rig.admin +
		"\r\nContent-Type: application/octet-stream\r\nContent-Length: 1000000\r\n\r\nsome bytes"
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the upload never started")
	}
	_ = conn.Close()
	select {
	case err := <-observed:
		if err == nil {
			t.Fatal("the upload read a clean end from a client that left")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the upload never noticed the client leaving")
	}
}

func TestDownloadStorageFileFile(t *testing.T) {
	rig := newStorageFilesRig(t)
	rig.streams.download = func(context.Context, string, proto.StreamHeader) (proto.StorageDownloadHead, io.ReadCloser, error) {
		return proto.StorageDownloadHead{Size: 5, Name: "a.txt"}, io.NopCloser(strings.NewReader("hello")), nil
	}
	rec := rig.do(rig.admin, http.MethodGet, rig.url(rig.shared, "/download?path=files/a.txt"), nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "hello" {
		t.Fatalf("download = %d %q", rec.Code, rec.Body)
	}
	h := rec.Header()
	if h.Get("Content-Length") != "5" || h.Get("Content-Type") != "application/octet-stream" || h.Get("Content-Disposition") != "attachment; filename=a.txt" {
		t.Fatalf("headers = %v", h)
	}
	_, hdr := rig.streams.lastHeader(t)
	if hdr.StorageID != rig.shared.ID || hdr.Rel != "files/a.txt" || hdr.Format != proto.StorageArchiveZip {
		t.Fatalf("header = %+v", hdr)
	}
	// A name outside ASCII travels as RFC 2231 filename*.
	rig.streams.download = func(context.Context, string, proto.StreamHeader) (proto.StorageDownloadHead, io.ReadCloser, error) {
		return proto.StorageDownloadHead{Size: 2, Name: "数据 集.json"}, io.NopCloser(strings.NewReader("{}")), nil
	}
	rec = rig.do(rig.admin, http.MethodGet, rig.url(rig.shared, "/download?path=files/x"), nil)
	if got := rec.Header().Get("Content-Disposition"); got != "attachment; filename*=utf-8''%E6%95%B0%E6%8D%AE%20%E9%9B%86.json" {
		t.Fatalf("disposition = %q", got)
	}
}

func TestDownloadStorageFileDirectoryArchives(t *testing.T) {
	rig := newStorageFilesRig(t)
	rig.streams.download = func(_ context.Context, _ string, hdr proto.StreamHeader) (proto.StorageDownloadHead, io.ReadCloser, error) {
		return proto.StorageDownloadHead{Dir: true, Size: -1, Name: "results"}, io.NopCloser(strings.NewReader("packed:" + hdr.Format)), nil
	}
	for format, want := range map[string][3]string{
		"":       {"application/zip", "attachment; filename=results.zip", "packed:zip"},
		"zip":    {"application/zip", "attachment; filename=results.zip", "packed:zip"},
		"tar.gz": {"application/gzip", "attachment; filename=results.tar.gz", "packed:tar.gz"},
	} {
		rec := rig.do(rig.admin, http.MethodGet, rig.url(rig.local, "/download?node_uuid="+rig.n2.UUID+"&path=.gpuworks/apps/eval/results&format="+format), nil)
		h := rec.Header()
		if rec.Code != http.StatusOK || h.Get("Content-Type") != want[0] || h.Get("Content-Disposition") != want[1] || rec.Body.String() != want[2] || h.Get("Content-Length") != "" {
			t.Fatalf("format %q = %d %v %q", format, rec.Code, h, rec.Body)
		}
	}
}

// The whole storage downloads as one archive, named as the agent names it,
// or after the storage when the agent gives no name.
func TestDownloadStorageFileRoot(t *testing.T) {
	rig := newStorageFilesRig(t)
	name := "storage"
	rig.streams.download = func(context.Context, string, proto.StreamHeader) (proto.StorageDownloadHead, io.ReadCloser, error) {
		return proto.StorageDownloadHead{Dir: true, Size: -1, Name: name}, io.NopCloser(strings.NewReader("zip")), nil
	}
	rec := rig.do(rig.admin, http.MethodGet, rig.url(rig.shared, "/download"), nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Disposition") != "attachment; filename=storage.zip" {
		t.Fatalf("root = %d %v", rec.Code, rec.Header())
	}
	if _, hdr := rig.streams.lastHeader(t); hdr.Rel != "" || hdr.Format != proto.StorageArchiveZip {
		t.Fatalf("header = %+v", hdr)
	}
	name = ""
	rec = rig.do(rig.admin, http.MethodGet, rig.url(rig.shared, "/download?path=/&format=tar.gz"), nil)
	if rec.Header().Get("Content-Disposition") != "attachment; filename=team.tar.gz" {
		t.Fatalf("unnamed root = %v", rec.Header())
	}
}

func TestDownloadStorageFileRefusals(t *testing.T) {
	rig := newStorageFilesRig(t)
	expectError(t, rig.do(rig.admin, http.MethodGet, rig.url(rig.shared, "/download?path=files/none"), nil), http.StatusNotFound, ErrorStorageFileNotFound)
	rig.streams.download = func(context.Context, string, proto.StreamHeader) (proto.StorageDownloadHead, io.ReadCloser, error) {
		return proto.StorageDownloadHead{Code: proto.StorageFilesErrPathInvalid, Error: "symlink leaves the root"}, nil, nil
	}
	expectError(t, rig.do(rig.admin, http.MethodGet, rig.url(rig.shared, "/download?path=files/ln"), nil), http.StatusBadRequest, ErrorStorageFilePathInvalid)
	rig.streams.download = func(context.Context, string, proto.StreamHeader) (proto.StorageDownloadHead, io.ReadCloser, error) {
		return proto.StorageDownloadHead{}, nil, errors.New("agent did not answer the download")
	}
	expectError(t, rig.do(rig.admin, http.MethodGet, rig.url(rig.shared, "/download?path=files/a"), nil), http.StatusBadGateway, ErrorStorageFileOperationFailed)

	opened := rig.streams.opened()
	expectError(t, rig.do(rig.admin, http.MethodGet, rig.url(rig.shared, "/download?path=files/../x"), nil), http.StatusBadRequest, ErrorStorageFilePathInvalid)
	expectError(t, rig.do(rig.admin, http.MethodGet, rig.url(rig.shared, "/download?path=files/d&format=rar"), nil), http.StatusBadRequest, ErrorFieldInvalid)
	expectError(t, rig.do(rig.admin, http.MethodGet, rig.url(rig.local, "/download?path=files/a"), nil), http.StatusBadRequest, ErrorStorageFileNodeRequired)
	rig.streams.setAttached(rig.n1.UUID, false)
	expectError(t, rig.do(rig.admin, http.MethodGet, rig.url(rig.local, "/download?node_uuid="+rig.n1.UUID+"&path=files/a"), nil), http.StatusServiceUnavailable, ErrorStorageFileDataLaneUnavailable)
	if rig.streams.opened() != opened {
		t.Fatal("a refused download opened a stream")
	}
	// A disabled storage still downloads.
	rig.streams.setAttached(rig.n1.UUID, true)
	rig.disable()
	rig.streams.download = func(context.Context, string, proto.StreamHeader) (proto.StorageDownloadHead, io.ReadCloser, error) {
		return proto.StorageDownloadHead{Size: 1, Name: "a"}, io.NopCloser(strings.NewReader("a")), nil
	}
	if rec := rig.do(rig.admin, http.MethodGet, rig.url(rig.shared, "/download?path=files/a"), nil); rec.Code != http.StatusOK {
		t.Fatalf("download from a disabled storage = %d %s", rec.Code, rec.Body)
	}
}

type brokenBody struct {
	sent   bool
	closed chan struct{}
}

func (b *brokenBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, "partial"), nil
	}
	return 0, errors.New("stream reset")
}

func (b *brokenBody) Close() error { close(b.closed); return nil }

// Once bytes flow, a failure cuts the connection: the client sees an
// incomplete download, never a complete-looking one, and the stream is closed.
func TestDownloadStorageFileCutMidway(t *testing.T) {
	for _, dir := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "dir"}[dir], func(t *testing.T) { cutDownload(t, dir) })
	}
}

func cutDownload(t *testing.T, dir bool) {
	rig := newStorageFilesRig(t)
	body := &brokenBody{closed: make(chan struct{})}
	rig.streams.download = func(context.Context, string, proto.StreamHeader) (proto.StorageDownloadHead, io.ReadCloser, error) {
		if dir {
			return proto.StorageDownloadHead{Dir: true, Size: -1, Name: "d"}, body, nil
		}
		return proto.StorageDownloadHead{Size: 1000, Name: "a"}, body, nil
	}
	srv := httptest.NewServer(rig.h)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+rig.url(rig.shared, "/download?path=files/a"), nil)
	req.Header.Set("Authorization", "Bearer "+rig.admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("dir=%v: the headers are flushed before the bytes: %v", dir, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dir=%v: status %d", dir, resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil {
		t.Fatalf("dir=%v: a cut download read as complete: %q", dir, got)
	}
	select {
	case <-body.closed:
	case <-time.After(5 * time.Second):
		t.Fatalf("dir=%v: the download stream was not closed", dir)
	}
}
