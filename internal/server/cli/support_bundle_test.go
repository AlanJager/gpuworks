package cli

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// memberLayout is a control-plane cluster member's installation: data with
// the enrollment configuration and no gpuworks.db, the log beside it.
func memberLayout(t *testing.T) (install, data string) {
	t.Helper()
	install = t.TempDir()
	data = filepath.Join(install, "data")
	if err := os.MkdirAll(filepath.Join(data, "cluster-enrollment"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "cluster-enrollment", "config.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return install, data
}

func runSupportBundle(t *testing.T, args ...string) (int, string) {
	t.Helper()
	// The command's failure is reported through the logger.
	var stderr bytes.Buffer
	code := Run(slog.New(slog.NewTextHandler(&stderr, nil)), append([]string{"support-bundle"}, args...), &stderr, Options{})
	return code, stderr.String()
}

// What the command does with a member's data directory depends on the
// build: support_bundle_community_test.go and support_bundle_cluster_test.go
// each assert their own. The cases below hold for both.

func TestSupportBundleOnAClusterMemberRefusesWhatOnlyTheDatabaseHolds(t *testing.T) {
	_, data := memberLayout(t)
	for _, flag := range [][]string{{"--deployments", "3"}, {"--instances", "abc"}} {
		code, stderr := runSupportBundle(t, append([]string{"--data", data}, flag...)...)
		if code == 0 || !strings.Contains(stderr, "read the cluster database") || !strings.Contains(stderr, "Settings > Diagnostics") {
			t.Fatalf("%v: exit %d:\n%s", flag, code, stderr)
		}
	}
}

func TestSupportBundleLocalIsForAClusterMember(t *testing.T) {
	code, stderr := runSupportBundle(t, "--data", t.TempDir(), "--local")
	if code == 0 || !strings.Contains(stderr, "--local applies to a control-plane cluster member") {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
}

func TestSupportBundleRefusesAWindowItCannotRead(t *testing.T) {
	_, data := memberLayout(t)
	code, stderr := runSupportBundle(t, "--data", data, "--from", "yesterday", "--to", "2026-11-02T12:00:00Z")
	if code == 0 || !strings.Contains(stderr, "--from") {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
}

// The log is where the installation that made the member keeps it.
func TestMemberLogFileFollowsTheInstallation(t *testing.T) {
	install, data := memberLayout(t)
	if got := memberLogFile(data); got != defaultLogFile(data) {
		t.Fatalf("with no log on disk = %s, want the default", got)
	}
	_ = os.MkdirAll(filepath.Join(install, "logs"), 0o700)
	cluster := filepath.Join(install, "logs", "server.log")
	if err := os.WriteFile(cluster, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := memberLogFile(data); got != cluster {
		t.Fatalf("cluster installation = %s, want %s", got, cluster)
	}
	_ = os.MkdirAll(filepath.Dir(defaultLogFile(data)), 0o700)
	if err := os.WriteFile(defaultLogFile(data), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := memberLogFile(data); got != defaultLogFile(data) {
		t.Fatalf("with the default present = %s", got)
	}
}
