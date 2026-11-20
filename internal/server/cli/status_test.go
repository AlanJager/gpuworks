//go:build linux && dqlite

package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/app"
)

func TestStatusCommandReportsCachedSpareWithoutOffline(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "gpuworks.db.dqlite")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"info.yaml":    "ID: 7\nAddress: 127.0.0.1:1\nRole: 0\n",
		"cluster.yaml": "- ID: 7\n  Address: 127.0.0.1:1\n  Role: 2\n- ID: 8\n  Address: 127.0.0.1:2\n  Role: 0\n",
		"dqlite-lock":  "",
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var stdout bytes.Buffer
	err := statusCommand(context.Background(), []string{"--data", root, "--timeout", "200ms"}, &stdout)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"Role: spare (cluster.yaml cache)", "Voters: 1", "Fault tolerant: false", "Sync: not replicating", "Commit index: not observed"} {
		if !strings.Contains(stdout.String(), line) {
			t.Fatalf("missing %q\n%s", line, stdout.String())
		}
	}
}

func TestRunUsageIncludesStatus(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(discardLog(), nil, &stderr); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr.String(), "gpuworks cluster-status [--data ./data] [--http-addr HOST:PORT] [--timeout 5s]") {
		t.Fatalf("usage has no status command:\n%s", stderr.String())
	}
}

func TestStatusRejectsPositionalArguments(t *testing.T) {
	err := statusCommand(context.Background(), []string{"extra"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "positional") {
		t.Fatal(err)
	}
}

func TestStatusCommandPrintsObservedBusinessFailure(t *testing.T) {
	root := writeStatusCommandIdentity(t)
	business := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/readyz" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("unexpected business probe: %s %s", r.Method, r.URL.Path)
		}
		http.Error(w, "member business service is not ready", http.StatusServiceUnavailable)
	}))
	t.Cleanup(business.Close)
	var stdout bytes.Buffer
	err := statusCommand(t.Context(), []string{"--data", root, "--http-addr", business.Listener.Addr().String(), "--timeout", "200ms"}, &stdout)
	if !errors.Is(err, app.ErrBusinessStatusNotReady) {
		t.Fatalf("observed HTTP 503 did not propagate readiness failure: %v", err)
	}
	for _, line := range []string{"Status report", "Business: member business service is not ready", "Member: 7 "} {
		if !strings.Contains(stdout.String(), line) {
			t.Fatalf("failure discarded report line %q: %v\n%s", line, err, stdout.String())
		}
	}
}

func TestStatusCommandPrefersReportWriteFailure(t *testing.T) {
	root := writeStatusCommandIdentity(t)
	business := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "member business service is not ready", http.StatusServiceUnavailable)
	}))
	t.Cleanup(business.Close)
	reader, writer := io.Pipe()
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	err := statusCommand(t.Context(), []string{"--data", root, "--http-addr", business.Listener.Addr().String(), "--timeout", "200ms"}, writer)
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("report write failure was hidden by observation error: %v", err)
	}
}

func TestStatusCommandWithoutHTTPAddressRemainsUnverified(t *testing.T) {
	var stdout bytes.Buffer
	err := statusCommand(t.Context(), []string{"--data", writeStatusCommandIdentity(t), "--timeout", "200ms"}, &stdout)
	if err != nil {
		t.Fatalf("identity-only status compatibility changed: %v", err)
	}
	for _, line := range []string{"Overall: unverified", "--http-addr", "Business: not observed"} {
		if !strings.Contains(stdout.String(), line) {
			t.Fatalf("missing unverified explanation %q\n%s", line, stdout.String())
		}
	}
}

func writeStatusCommandIdentity(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "gpuworks.db.dqlite")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		"info.yaml":    "ID: 7\nAddress: 127.0.0.1:1\nRole: 0\n",
		"cluster.yaml": "- ID: 7\n  Address: 127.0.0.1:1\n  Role: 0\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
