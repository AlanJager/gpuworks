package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

type storageLogBuffer struct {
	sync.Mutex
	bytes.Buffer
}

func (b *storageLogBuffer) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.Buffer.Write(p)
}
func (b *storageLogBuffer) text() string { b.Lock(); defer b.Unlock(); return b.Buffer.String() }
func storageFailureLog(t *testing.T, b *storageLogBuffer) map[string]any {
	t.Helper()
	for _, line := range strings.Split(b.text(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["msg"] == "storage file download cut short" {
			return m
		}
	}
	t.Fatalf("missing failure log: %s", b.text())
	return nil
}

func TestDownloadStorageFileSourceFailureEvidence(t *testing.T) {
	var logs storageLogBuffer
	rig := newStorageFilesRig(t, slog.New(slog.NewJSONHandler(&logs, nil)))
	body := &brokenBody{closed: make(chan struct{})}
	rig.streams.download = func(context.Context, string, proto.StreamHeader) (proto.StorageDownloadHead, io.ReadCloser, error) {
		return proto.StorageDownloadHead{Size: 1000, Name: "a"}, body, nil
	}
	srv := httptest.NewServer(rig.h)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+rig.url(rig.shared, "/download?path=files/a"), nil)
	req.Header.Set("Authorization", "Bearer "+rig.admin)
	req.Header.Set("X-Request-ID", "source-cut-evidence")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	<-body.closed
	if resp.StatusCode != 200 || readErr == nil || len(got) > len("partial") {
		t.Fatalf("download falsely complete: status=%d body=%q err=%v", resp.StatusCode, got, readErr)
	}
	record := storageFailureLog(t, &logs)
	if resp.Header.Get("X-Request-ID") != "source-cut-evidence" || record["request_id"] != "source-cut-evidence" || record["direction"] != "node_to_gateway" || record["stage"] != "source_read" || record["bytes"] != float64(7) || record["expected_bytes"] != float64(1000) || record["node"] != rig.n1.UUID {
		t.Fatalf("source evidence header=%s record=%v", resp.Header.Get("X-Request-ID"), record)
	}
	if strings.Contains(logs.text(), rig.admin) {
		t.Fatal("credential leaked")
	}
}

type failedDownloadWriter struct {
	header http.Header
	status int
	writes int
}

func (w *failedDownloadWriter) Header() http.Header { return w.header }
func (w *failedDownloadWriter) WriteHeader(n int)   { w.status = n }
func (w *failedDownloadWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, errors.New("browser connection closed")
}
func (w *failedDownloadWriter) Flush() {}
func TestDownloadStorageFileClientFailureEvidence(t *testing.T) {
	var logs storageLogBuffer
	rig := newStorageFilesRig(t, slog.New(slog.NewJSONHandler(&logs, nil)))
	closed := false
	rig.streams.download = func(context.Context, string, proto.StreamHeader) (proto.StorageDownloadHead, io.ReadCloser, error) {
		return proto.StorageDownloadHead{Size: 7, Name: "a"}, &closeAwareReader{Reader: strings.NewReader("content"), closed: &closed}, nil
	}
	req := httptest.NewRequest(http.MethodGet, rig.url(rig.shared, "/download?path=files/a"), nil)
	req.Header.Set("Authorization", "Bearer "+rig.admin)
	req.Header.Set("X-Request-ID", "client-cut-evidence")
	w := &failedDownloadWriter{header: make(http.Header)}
	func() {
		defer func() {
			if got := recover(); got != http.ErrAbortHandler {
				t.Errorf("panic=%v", got)
			}
		}()
		rig.h.ServeHTTP(w, req)
	}()
	record := storageFailureLog(t, &logs)
	if !closed || w.status != 200 || w.writes != 1 || record["request_id"] != "client-cut-evidence" || record["direction"] != "gateway_to_client" || record["stage"] != "client_write" || record["expected_bytes"] != float64(7) {
		t.Fatalf("client evidence closed=%v status=%d writes=%d record=%v", closed, w.status, w.writes, record)
	}
}

type closeAwareReader struct {
	io.Reader
	closed *bool
}

func (r *closeAwareReader) Close() error { *r.closed = true; return nil }

func TestDownloadStorageFileLengthMismatchEvidence(t *testing.T) {
	var logs storageLogBuffer
	rig := newStorageFilesRig(t, slog.New(slog.NewJSONHandler(&logs, nil)))
	closed := false
	rig.streams.download = func(context.Context, string, proto.StreamHeader) (proto.StorageDownloadHead, io.ReadCloser, error) {
		return proto.StorageDownloadHead{Size: 99, Name: "a"}, &closeAwareReader{Reader: strings.NewReader("short"), closed: &closed}, nil
	}
	req := httptest.NewRequest(http.MethodGet, rig.url(rig.shared, "/download?path=files/a"), nil)
	req.Header.Set("Authorization", "Bearer "+rig.admin)
	out := httptest.NewRecorder()
	func() {
		defer func() {
			if got := recover(); got != http.ErrAbortHandler {
				t.Errorf("panic=%v", got)
			}
		}()
		rig.h.ServeHTTP(out, req)
	}()
	record := storageFailureLog(t, &logs)
	if !closed || out.Code != 200 || out.Body.String() != "short" || record["stage"] != "response_length" || record["direction"] != "node_to_gateway" || record["bytes"] != float64(5) || record["expected_bytes"] != float64(99) || out.Header().Get("X-Request-ID") == "" {
		t.Fatalf("length mismatch evidence closed=%v status=%d body=%q log=%v", closed, out.Code, out.Body, record)
	}
}
