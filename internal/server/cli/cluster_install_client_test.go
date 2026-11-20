package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClusterClientRequiresBinaryBeforePasswordAndSSH(t *testing.T) {
	for _, command := range []string{"cluster-install", "cluster-check"} {
		for _, explicitEmpty := range []bool{false, true} {
			t.Run(command+map[bool]string{false: "/omitted", true: "/empty"}[explicitEmpty], func(t *testing.T) {
				sshLog := clusterPasswordPromptSSHFailure(t)
				config := writeClusterConfig(t, clusterPasswordPromptConfig)
				args := []string{"--config", config}
				if explicitEmpty {
					args = append(args, "--binary=")
				}
				streams, output := clusterWizardIO("private-password", "private-password")
				input := streams.In.(*strings.Reader)
				before := input.Len()
				var err error
				if command == "cluster-install" {
					err = clusterInstallCommandWithIO(t.Context(), args, streams)
				} else {
					err = clusterCheckCommand(t.Context(), args, output)
				}
				if err == nil || !strings.Contains(err.Error(), "--binary is required") {
					t.Fatalf("missing actionable runtime selection error: %v", err)
				}
				if input.Len() != before || strings.Contains(output.String(), "初始管理员密码") {
					t.Fatal("missing runtime prompted for or consumed a password")
				}
				if _, err := os.Stat(sshLog); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("missing runtime reached SSH")
				}
			})
		}
	}
}

func TestClusterWizardRetainsConfigurationWhenRuntimeIsMissing(t *testing.T) {
	for _, choice := range []string{"2", "3"} {
		t.Run(choice, func(t *testing.T) {
			clusterWizardSSHFixture(t, false)
			config := filepath.Join(t.TempDir(), "cluster.yaml")
			lines := append(clusterWizardDefaults(), "y", choice, "private-password", "private-password")
			streams, output := clusterWizardIO(lines...)
			err := runClusterInstallWizard(t.Context(), []string{"--interactive", "--config", config}, streams, clusterWizardNoExecute(t))
			if err == nil || !strings.Contains(err.Error(), "--binary is required") {
				t.Fatalf("missing runtime was accepted: %v", err)
			}
			clusterWizardReadConfig(t, config)
			if strings.Contains(output.String(), "初始管理员密码") || strings.Contains(output.String(), "管理员密码文件") {
				t.Fatal("missing runtime requested credentials")
			}
			entries, err := os.ReadDir(filepath.Dir(config))
			if err != nil || len(entries) != 1 || entries[0].Name() != "cluster.yaml" {
				t.Fatalf("missing runtime created credentials: entries=%v err=%v", entries, err)
			}
		})
	}
}
