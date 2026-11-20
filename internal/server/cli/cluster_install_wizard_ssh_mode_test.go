package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestClusterInstallWizardCommonSSHAskedOnce(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	key := filepath.Join(t.TempDir(), "shared.key")
	if err := os.WriteFile(key, []byte("test-only-key"), 0600); err != nil {
		t.Fatal(err)
	}
	streams, out := clusterWizardIO("2", "192.0.2.11", "192.0.2.12", "192.0.2.13", "admin", "2222", key, "0", "y")
	if err := runClusterInstallWizard(t.Context(), []string{"--configure", "--config", path}, streams, clusterWizardNoExecute(t)); err != nil {
		t.Fatalf("common SSH: %v; output=%s", err, out.String())
	}
	for _, node := range clusterWizardReadConfig(t, path).Nodes {
		want := clusterInstallSSH{Target: node.Address, User: "admin", Port: 2222, Key: key}
		if !reflect.DeepEqual(node.SSH, want) || node.Dir != "/opt/gpuworks/server" {
			t.Fatalf("common SSH configuration not applied: %+v", node)
		}
		if !strings.Contains(out.String(), "admin@"+node.Address+":2222") {
			t.Fatalf("summary ignored common configuration: %s", out.String())
		}
	}
	for _, field := range []string{"SSH 账号（", "SSH 端口（", "SSH 私钥（"} {
		if strings.Count(out.String(), field) != 1 {
			t.Fatalf("common %s must be asked exactly once: %s", field, out.String())
		}
	}
	if strings.Contains(out.String(), "共用安装目录") || strings.Contains(out.String(), "节点 1 SSH 地址或别名 [") {
		t.Fatalf("common flow asked unnecessary per-node fields: %s", out.String())
	}
}

func TestClusterInstallWizardCommonSSHCanUseLocalDefaults(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	streams, out := clusterWizardIO("2", "192.0.2.11", "192.0.2.12", "192.0.2.13", "", "", "", "0", "y")
	if err := runClusterInstallWizard(t.Context(), []string{"--configure", "--config", path}, streams, clusterWizardNoExecute(t)); err != nil {
		t.Fatalf("common defaults: %v; output=%s", err, out.String())
	}
	for _, node := range clusterWizardReadConfig(t, path).Nodes {
		if node.SSH.User != "operator" || node.SSH.Port != 22 || node.SSH.Key != "" {
			t.Fatalf("common default values did not use actual local settings: %+v", node.SSH)
		}
	}
}

func TestClusterInstallWizardCommonSSHCanClearExistingOverrides(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	clusterWizardExistingConfig(t, path)
	streams, out := clusterWizardIO("2", "", "", "", "-", "-", "-", "0", "y", "y")
	if err := runClusterInstallWizard(t.Context(), []string{"--configure", "--config", path}, streams, clusterWizardNoExecute(t)); err != nil {
		t.Fatalf("common clear: %v; output=%s", err, out.String())
	}
	for i, node := range clusterWizardReadConfig(t, path).Nodes {
		if node.SSH.User != "" || node.SSH.Port != 0 || node.SSH.Key != "" {
			t.Fatalf("cleared common SSH overrides persisted: %+v", node.SSH)
		}
		if node.SSH.Target == node.Address || !strings.HasPrefix(node.Dir, "/srv/") || node.Name != []string{"first", "second", "third"}[i] {
			t.Fatalf("clearing SSH settings changed unrelated node settings: %+v", node)
		}
	}
}

func TestClusterInstallWizardPerNodeSSHStrategy(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	key := filepath.Join(t.TempDir(), "first.key")
	if err := os.WriteFile(key, []byte("test-only-first-key"), 0600); err != nil {
		t.Fatal(err)
	}
	streams, out := clusterWizardIO("3", "192.0.2.11", "192.0.2.12", "192.0.2.13",
		"first-alias", "admin", "2222", key,
		"second-alias", "root", "2203", "-",
		"third-alias", "-", "-", "-", "0", "y")
	if err := runClusterInstallWizard(t.Context(), []string{"--configure", "--config", path}, streams, clusterWizardNoExecute(t)); err != nil {
		t.Fatalf("per-node SSH: %v; output=%s", err, out.String())
	}
	want := []clusterInstallNode{
		{Name: "control-1", Address: "192.0.2.11", Dir: "/opt/gpuworks/server", SSH: clusterInstallSSH{Target: "first-alias", User: "admin", Port: 2222, Key: key}},
		{Name: "control-2", Address: "192.0.2.12", Dir: "/opt/gpuworks/server", SSH: clusterInstallSSH{Target: "second-alias", User: "root", Port: 2203}},
		{Name: "control-3", Address: "192.0.2.13", Dir: "/opt/gpuworks/server", SSH: clusterInstallSSH{Target: "third-alias"}},
	}
	if got := clusterWizardReadConfig(t, path).Nodes; !reflect.DeepEqual(got, want) {
		t.Fatalf("different SSH settings lost: got=%+v want=%+v", got, want)
	}
	for _, field := range []string{"SSH 地址或别名 [", "SSH 账号（", "SSH 端口（", "SSH 私钥（"} {
		if strings.Count(out.String(), field) != 3 {
			t.Fatalf("per-node %s must be asked for each node: %s", field, out.String())
		}
	}
}

func TestClusterInstallWizardSSHStrategyRetriesAndDefault(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	lines := append([]string{"invalid", "0", "4", ""}, clusterWizardDefaults()[1:]...)
	streams, out := clusterWizardIO(append(lines, "y")...)
	if err := runClusterInstallWizard(t.Context(), []string{"--configure", "--config", path}, streams, clusterWizardNoExecute(t)); err != nil {
		t.Fatalf("strategy retries: %v; output=%s", err, out.String())
	}
	for _, node := range clusterWizardReadConfig(t, path).Nodes {
		if node.SSH.User != "" || node.SSH.Port != 0 || node.SSH.Key != "" {
			t.Fatalf("default strategy must preserve automatic SSH identity: %+v", node.SSH)
		}
	}
	if strings.Count(out.String(), "输入无效") != 3 {
		t.Fatalf("invalid SSH strategies were not retried: %s", out.String())
	}
}
