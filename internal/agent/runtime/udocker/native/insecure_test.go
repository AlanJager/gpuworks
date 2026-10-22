package native

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// plainCopy serves reg's handler over plain HTTP as well.
func plainCopy(t *testing.T, reg *fakeRegistry) string {
	t.Helper()
	plain := httptest.NewServer(reg.server.Config.Handler)
	t.Cleanup(plain.Close)
	return strings.TrimPrefix(plain.URL, "http://")
}

// untrustingEngine uses the production client: it does not trust the fake
// registry's self-signed certificate.
func untrustingEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	dir := t.TempDir()
	engine := &Engine{Layout: Layout{Dir: dir}, Client: NewClient(), Workers: 2}
	t.Cleanup(engine.WaitBackground)
	return engine, dir
}

func TestPullFromPlainHTTPLoopbackRegistry(t *testing.T) {
	reg := newFakeRegistry(t, "team/app", [][]byte{buildLayer(t, true, []entry{{name: "a.txt", body: "a"}})}, []byte(`{"config":{}}`))
	host := plainCopy(t, reg)
	engine, dir := untrustingEngine(t)
	image := host + "/team/app:latest"
	if err := engine.Pull(context.Background(), image, nil); err != nil {
		t.Fatalf("pull from a plain-HTTP loopback registry: %v", err)
	}
	if err := engine.Create(context.Background(), "c", image, nil); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "containers", "c", "ROOT", "a.txt")); err != nil || string(data) != "a" {
		t.Fatalf("rootfs: %q %v", data, err)
	}
	if engine.Client.knownBaseURL(host) != "http://"+host {
		t.Fatalf("scheme not remembered: %s", engine.Client.knownBaseURL(host))
	}
}

func TestPullNeverDowngradesAVerifiedRegistry(t *testing.T) {
	reg := newFakeRegistry(t, "team/app", [][]byte{buildLayer(t, true, []entry{{name: "a.txt", body: "a"}})}, []byte(`{"config":{}}`))
	host := plainCopy(t, reg)
	engine, _ := untrustingEngine(t)
	engine.Client.Insecure = func(string) bool { return false }
	err := engine.Pull(context.Background(), host+"/team/app:latest", nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP response to HTTPS client") {
		t.Fatalf("a verified registry was reached over plain HTTP: %v", err)
	}
}

func TestPullSkipsCertificateVerificationOnlyForInsecureRegistries(t *testing.T) {
	reg := newFakeRegistry(t, "team/app", [][]byte{buildLayer(t, true, []entry{{name: "a.txt", body: "a"}})}, []byte(`{"config":{}}`))
	engine, _ := untrustingEngine(t)
	if err := engine.Pull(context.Background(), reg.image(), nil); err != nil {
		t.Fatalf("self-signed loopback registry: %v", err)
	}
	strict, _ := untrustingEngine(t)
	strict.Client.Insecure = func(string) bool { return false }
	if err := strict.Pull(context.Background(), reg.image(), nil); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("unverified certificate accepted for a verified registry: %v", err)
	}
}

func TestInsecureRegistryMatching(t *testing.T) {
	const configured = "reg.example:5000, plainhost ,10.1.0.0/16,bad-cidr/99"
	for host, want := range map[string]bool{
		"127.0.0.1:5000":         true,
		"localhost:5000":         true,
		"[::1]:5000":             true,
		"reg.example:5000":       true,
		"reg.example:6000":       false,
		"plainhost":              true,
		"plainhost:8080":         true,
		"10.1.2.3:5000":          true,
		"10.2.0.1:5000":          false,
		"registry-1.docker.io":   false,
		"quay.io":                false,
		"notplainhost.example:1": false,
	} {
		if got := insecureRegistry(host, configured); got != want {
			t.Errorf("insecureRegistry(%q) = %v, want %v", host, got, want)
		}
	}
	if insecureRegistry("quay.io", "") {
		t.Error("public registry insecure without configuration")
	}
}
