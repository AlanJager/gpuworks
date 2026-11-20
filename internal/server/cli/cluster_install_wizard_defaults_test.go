package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClusterInstallWizardPortDefaultsMatchEffectiveOpenSSH(t *testing.T) {
	for _, mode := range []string{"2", "3"} {
		for _, port := range []int{22, 2222} {
			t.Run(fmt.Sprintf("mode%s-port%d", mode, port), func(t *testing.T) {
				clusterWizardSSHFixture(t, false)
				config := fmt.Sprintf("Host 192.0.2.*\n User ubuntu\n Port %d\n IdentityFile none\n", port)
				if err := os.WriteFile(os.Getenv("GPUWORKS_WIZARD_SSH_CONFIG"), []byte(config), 0600); err != nil {
					t.Fatal(err)
				}
				lines := []string{mode, "192.0.2.11", "192.0.2.12", "192.0.2.13"}
				count := 1
				if mode == "3" {
					count = 3
				}
				for range count {
					if mode == "3" {
						lines = append(lines, "") // accept node SSH target
					}
					lines = append(lines, "", "", "")
				}
				lines = append(lines, "0", "y")
				streams, out := clusterWizardIO(lines...)
				path := filepath.Join(t.TempDir(), "cluster.yaml")
				if err := runClusterInstallWizard(t.Context(), []string{"--configure", "--config", path}, streams, clusterWizardNoExecute(t)); err != nil {
					t.Fatalf("wizard: %v; output=%s", err, out.String())
				}
				label := fmt.Sprintf("SSH 端口（自动沿用 SSH config） [%d]:", port)
				if strings.Count(out.String(), label) != count || strings.Contains(out.String(), "[-]:") {
					t.Fatalf("resolved defaults missing: %s", out.String())
				}
				for _, node := range clusterWizardReadConfig(t, path).Nodes {
					if node.SSH.Port != port || node.SSH.User != "ubuntu" || node.SSH.Key != "" {
						t.Fatalf("Enter did not accept shown defaults: %+v", node.SSH)
					}
				}
			})
		}
	}
}

func TestClusterInstallWizardKeyDefaultsUseAvailableHostIdentities(t *testing.T) {
	for _, kind := range []string{"ordered", "missing", "none", "home", "public-only", "explicit", "unsupported-token"} {
		t.Run(kind, func(t *testing.T) {
			clusterWizardSSHFixture(t, false)
			dir := t.TempDir()
			key := filepath.Join(dir, "available.key")
			second := filepath.Join(dir, "second.key")
			for _, path := range []string{key, second, key + ".pub"} {
				if err := os.WriteFile(path, []byte("private-test-marker-never-print"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			identities := fmt.Sprintf(" IdentityFile %s\n IdentityFile %s\n IdentityFile %s\n IdentityFile %s\n", filepath.Join(dir, "missing"), dir, key, second)
			ssh := clusterInstallSSH{Target: "node-alias"}
			want := key
			switch kind {
			case "missing":
				identities, want = " IdentityFile /missing/gpuworks-test-key\n", ""
			case "none":
				identities, want = " IdentityFile none\n IdentityFile "+key+"\n", ""
			case "home":
				t.Setenv("HOME", dir)
				identities = " IdentityFile ~/available.key\n"
			case "public-only":
				identities, want = " IdentityFile "+key+".pub\n", ""
			case "explicit":
				ssh.Key, ssh.User, ssh.Port, want = second, "admin", 2203, second
			case "unsupported-token":
				identities, want = " IdentityFile %d/available.key\n", ""
			}
			config := "Host node-alias\n User ubuntu\n Port 2222\n" + identities
			if err := os.WriteFile(os.Getenv("GPUWORKS_WIZARD_SSH_CONFIG"), []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			streams, out := clusterWizardIO("", "", "")
			got, err := clusterWizardSSH(t.Context(), streams, "节点 1", ssh, false)
			if err != nil || got.Key != want {
				t.Fatalf("identity default=%+v, err=%v, want=%s; output=%s", got, err, want, out.String())
			}
			defaultText := want
			if want == "" {
				defaultText = "自动"
			}
			if !strings.Contains(out.String(), "SSH 私钥（自动使用 SSH config/agent） ["+defaultText+"]:") || strings.Contains(out.String(), "private-test-marker") {
				t.Fatalf("key suggestion missing or secret leaked: %s", out.String())
			}
			if kind == "explicit" && (got.User != "admin" || got.Port != 2203) {
				t.Fatalf("explicit account/port defaults replaced: %+v", got)
			}
			if kind != "explicit" && (got.User != "ubuntu" || got.Port != 2222) {
				t.Fatalf("host-specific default settings replaced: %+v", got)
			}
		})
	}
}

func TestClusterInstallWizardAutomaticInputClearsSSHOverrides(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	streams, out := clusterWizardIO("自动", "自动", "自动")
	got, err := clusterWizardSSH(t.Context(), streams, "节点 1", clusterInstallSSH{Target: "alias", User: "admin", Port: 2222, Key: "/missing/explicit.key"}, false)
	if err != nil || got.User != "" || got.Port != 0 || got.Key != "" {
		t.Fatalf("automatic SSH settings=%+v err=%v; output=%s", got, err, out.String())
	}
}

func TestClusterInstallWizardLoadedHomeDirectoriesRemainExplicit(t *testing.T) {
	for _, noService := range []bool{false, true} {
		t.Run(fmt.Sprintf("no-service=%v", noService), func(t *testing.T) {
			clusterWizardSSHFixture(t, false)
			path := filepath.Join(t.TempDir(), "cluster.yaml")
			raw := clusterWizardExistingConfig(t, path)
			raw = []byte(strings.ReplaceAll(string(raw), "/srv/first-new", "/home/admin/gpuworks-cluster-new"))
			raw = []byte(strings.ReplaceAll(string(raw), "/srv/second-new", "/home/operator/gpuworks-cluster-new"))
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			before := clusterWizardReadConfig(t, path)
			streams, out := clusterWizardIO("2", "", "", "", "root", "", "", "0", "y", "y")
			args := []string{"--configure", "--config", path}
			if noService {
				args = append(args, "--no-service")
			}
			if err := runClusterInstallWizard(t.Context(), args, streams, clusterWizardNoExecute(t)); err != nil {
				t.Fatalf("wizard: %v; output=%s", err, out.String())
			}
			for i, node := range clusterWizardReadConfig(t, path).Nodes {
				if node.Dir != before.Nodes[i].Dir || node.SSH.User != "root" {
					t.Fatalf("loaded directory overwritten when changing user: before=%+v after=%+v", before.Nodes[i], node)
				}
			}
		})
	}
}

func TestClusterInstallWizardNoServiceUpdatesOnlyFreshSuggestedDirs(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	lines := []string{"1", "192.0.2.11", "192.0.2.12", "192.0.2.13",
		"1", "", "admin", "", "", "",
		"1", "", "root", "", "", "",
		"2", "", "admin", "", "", "/srv/custom-cluster",
		"2", "", "root", "", "", "", "0", "y"}
	streams, out := clusterWizardIO(lines...)
	if err := runClusterInstallWizard(t.Context(), []string{"--configure", "--config", path, "--no-service"}, streams, clusterWizardNoExecute(t)); err != nil {
		t.Fatalf("wizard: %v; output=%s", err, out.String())
	}
	for i, want := range []string{"/root/gpuworks-cluster-new", "/srv/custom-cluster", "/home/operator/gpuworks-cluster-new"} {
		if got := clusterWizardReadConfig(t, path).Nodes[i].Dir; got != want {
			t.Fatalf("node %d directory=%s want=%s", i+1, got, want)
		}
	}
}

func TestClusterInstallWizardNoServiceLoadedOmittedDirUsesSSHUser(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	raw := clusterWizardExistingConfig(t, path)
	for _, dir := range []string{"/srv/first-new", "/srv/second-new", "/srv/third-new"} {
		raw = []byte(strings.ReplaceAll(string(raw), "    dir: "+dir+"\n", ""))
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	streams, out := clusterWizardIO("2", "", "", "", "root", "", "", "0", "y", "y")
	if err := runClusterInstallWizard(t.Context(), []string{"--configure", "--config", path, "--no-service"}, streams, clusterWizardNoExecute(t)); err != nil {
		t.Fatalf("wizard: %v; output=%s", err, out.String())
	}
	for _, node := range clusterWizardReadConfig(t, path).Nodes {
		if node.Dir != "/root/gpuworks-cluster-new" {
			t.Fatalf("omitted no-service directory must use SSH user's home: %+v", node)
		}
	}
}

func TestClusterInstallWizardUnknownSSHConfigStillSuggestsPort22(t *testing.T) {
	clusterWizardSSHFixture(t, true)
	streams, out := clusterWizardIO("", "", "")
	got, err := clusterWizardSSH(t.Context(), streams, "节点 1", clusterInstallSSH{Target: "alias"}, false)
	if err != nil || got.Port != 22 || got.User != "" || got.Key != "" || !strings.Contains(out.String(), "SSH 端口（自动沿用 SSH config） [22]:") {
		t.Fatalf("fallback defaults=%+v err=%v; output=%s", got, err, out.String())
	}
}
