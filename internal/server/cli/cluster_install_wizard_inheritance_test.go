package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func clusterWizardAssertNoSSH(t *testing.T) {
	t.Helper()
	raw, err := os.ReadFile(os.Getenv("GPUWORKS_WIZARD_SSH_CALLS"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(raw) != 0 {
		t.Fatalf("wizard invoked SSH before collecting valid input: %s", raw)
	}
}

func TestClusterInstallWizardInferredSettingsStayOutsideYAML(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	streams, out := clusterWizardIO(append(clusterWizardDefaults(), "")...)
	if err := runClusterInstallWizard(t.Context(), []string{"--configure", "--config", path}, streams, clusterWizardNoExecute(t)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"user:", "port:", "key:", "defaults:"} {
		if strings.Contains(string(raw), field) {
			t.Fatalf("inferred SSH setting %q was persisted: %s", field, raw)
		}
	}
	for _, unexpected := range []string{"共用 SSH", "节点 1 名称", "配置文件 [", "HTTP 端口 [", "Enrollment 端口 [", "Database 端口（", "免密登录已通过"} {
		if strings.Contains(out.String(), unexpected) {
			t.Fatalf("default path retained unnecessary prompt or claimed login: %s", out.String())
		}
	}
	if !strings.Contains(out.String(), "尚未验证免密登录") {
		t.Fatalf("local SSH expansion must not imply successful authentication: %s", out.String())
	}
	sshConfig, err := os.ReadFile(os.Getenv("GPUWORKS_WIZARD_SSH_CONFIG"))
	if err != nil || string(sshConfig) != "Host *\n  User operator\n  Port 22\n  IdentityFile none\n" {
		t.Fatalf("wizard changed existing OpenSSH configuration: %s; %v", sshConfig, err)
	}
}

func TestClusterInstallWizardLocalSSHConfigFailureCanBeEdited(t *testing.T) {
	clusterWizardSSHFixture(t, true)
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	lines := []string{"1", "192.0.2.11", "192.0.2.12", "192.0.2.13", "2", "second-alias", "admin", "2222", "-", "/srv/second-new", "0", "y"}
	streams, out := clusterWizardIO(lines...)
	if err := runClusterInstallWizard(t.Context(), []string{"--configure", "--config", path}, streams, clusterWizardNoExecute(t)); err != nil {
		t.Fatalf("local config error should allow editing: %v; output=%s", err, out.String())
	}
	o := clusterWizardReadConfig(t, path)
	want := clusterInstallNode{Name: "control-2", Address: "192.0.2.12", Dir: "/srv/second-new", SSH: clusterInstallSSH{Target: "second-alias", User: "admin", Port: 2222}}
	if !reflect.DeepEqual(o.Nodes[1], want) || o.Nodes[0].Dir != "/opt/gpuworks/server" || o.Nodes[2].Dir != "/opt/gpuworks/server" {
		t.Fatalf("manual override or unknown-user fallback lost: %+v", o.Nodes)
	}
	if !strings.Contains(out.String(), "尚未验证") {
		t.Fatalf("SSH configuration failure must not claim login: %s", out.String())
	}
}

func TestClusterInstallWizardAddressChangeFollowsIPButRetainsAlias(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	clusterWizardExistingConfig(t, path)
	var previous clusterInstallOptions
	if err := readClusterInstallConfig(path, &previous, nil); err != nil {
		t.Fatal(err)
	}
	previous.Nodes[0].SSH.Target = previous.Nodes[0].Address
	config := clusterInstallConfig{Version: 1, Nodes: previous.Nodes}
	config.Ports.HTTP, config.Ports.Enrollment, config.Ports.Database = 8802, 9445, 9011
	raw, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	streams, _ := clusterWizardIO("1", "192.0.2.31", "192.0.2.32", "", "0", "y", "y")
	if err := runClusterInstallWizard(t.Context(), []string{"--configure", "--config", path}, streams, clusterWizardNoExecute(t)); err != nil {
		t.Fatal(err)
	}
	o := clusterWizardReadConfig(t, path)
	if o.Nodes[0].SSH.Target != "192.0.2.31" || o.Nodes[1].SSH.Target != "second-alias" || o.Nodes[0].Name != "first" || o.Nodes[1].Dir != "/srv/second-new" {
		t.Fatalf("changed IP did not retain node intent: %+v", o.Nodes)
	}
}
