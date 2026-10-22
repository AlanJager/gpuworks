package main

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/distribution"
)

const sampleNotices = `{"schema":1,"notices":[{"id":"cve-2026-0001","severity":"critical","kind":"security",
"title":{"en":"Gateway CVE","zh":"网关漏洞"},"url":"https://gpuworks.ai/security","expires_at":"2026-12-01T00:00:00Z"}]}`

func TestNoticesSignsWhatServersAccept(t *testing.T) {
	dir := t.TempDir()
	in, out := filepath.Join(dir, "in.json"), filepath.Join(dir, "notices.json")
	if err := os.WriteFile(in, []byte(sampleNotices), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_NOTICES_KEY", base64.StdEncoding.EncodeToString(testPrivate.Seed()))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"notices", "-in", in, "-out", out, "-key-id", "test", "-key-env", "TEST_NOTICES_KEY"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	signed, _ := os.ReadFile(out)
	payload, keyID, err := distribution.Open(signed, distribution.Keys{"test": testPublic})
	if err != nil || keyID != "test" {
		t.Fatalf("open = %v %q", err, keyID)
	}
	n, err := distribution.ParseNotices(payload)
	if err != nil || len(n.Notices) != 1 || n.PublishedAt.IsZero() || time.Since(n.PublishedAt) > time.Minute {
		t.Fatalf("parsed = %+v, %v", n, err)
	}
}

func TestNoticesRefusesWhatServersWouldReject(t *testing.T) {
	_, _, err := signNotices([]byte(strings.Replace(sampleNotices, "https://", "http://", 1)), "test", testPrivate, time.Now())
	if err == nil {
		t.Fatal("signed a notice with a plain http link")
	}
	t.Setenv("TEST_NOTICES_KEY", "")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"notices", "-in", "x", "-out", "y", "-key-env", "TEST_NOTICES_KEY"}, nil, &stdout, &stderr); code == 0 {
		t.Fatal("signed without a key")
	}
}

func TestNoticesVerifyReadsEachSourceBack(t *testing.T) {
	now := time.Date(2026, 10, 22, 1, 0, 0, 0, time.UTC)
	fresh, _, err := signNotices([]byte(sampleNotices), "gpuworks-dist-1", testPrivate, now)
	if err != nil {
		t.Fatal(err)
	}
	stale, _, _ := signNotices([]byte(sampleNotices), "gpuworks-dist-1", testPrivate, now.Add(-time.Hour))
	serve := func(body []byte) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/notices.json" {
				http.NotFound(w, r)
				return
			}
			w.Write(body)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	dir := t.TempDir()
	expect := filepath.Join(dir, "notices.json")
	os.WriteFile(expect, fresh, 0o644)
	keys := distribution.Keys{"gpuworks-dist-1": testPublic}
	var stdout bytes.Buffer
	if err := verifyNotices([]string{serve(fresh).URL, serve(fresh).URL}, expect, keys, &stdout); err != nil {
		t.Fatalf("verify = %v", err)
	}
	if err := verifyNotices([]string{serve(fresh).URL, serve(stale).URL}, expect, keys, &stdout); err == nil || !strings.Contains(err.Error(), "published at") {
		t.Fatalf("a source still serving the old file passed: %v", err)
	}
	if err := verifyNotices([]string{serve(fresh).URL}, expect, distribution.TrustedKeys(), &stdout); err == nil {
		t.Fatal("a file signed with an untrusted key verified")
	}
}
