package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const clusterPasswordPromptConfig = `version: 1
defaults:
  ssh:
    user: ubuntu
nodes:
  - name: one
    address: 192.0.2.11
  - name: two
    address: 192.0.2.12
  - name: three
    address: 192.0.2.13
`

func clusterPasswordPromptSSHFailure(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "ssh.log")
	// A real subprocess marks the first SSH operation and then fails. This
	// proves that cancellation/configuration errors never enter the transport.
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$GPUWORKS_PROMPT_SSH_LOG\"\nexit 73\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("GPUWORKS_PROMPT_SSH_LOG", log)
	return log
}

func TestClusterInstallConfigPromptsWithoutEditingFilesOrRepeatingWizard(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "no password file", true: "nearby password is not reused"}[existing], func(t *testing.T) {
			log := clusterPasswordPromptSSHFailure(t)
			config := writeClusterConfig(t, clusterPasswordPromptConfig)
			before, _ := os.Stat(config)
			passwordPath := filepath.Join(filepath.Dir(config), "cluster-admin.password")
			if existing {
				if err := os.WriteFile(passwordPath, []byte("old-password"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			secret := "  new password with spaces  "
			streams, out := clusterWizardIO(secret, secret)
			err := clusterInstallCommandWithIO(t.Context(), []string{"--binary", "/tmp/gpuworks-ha", "--config", config}, streams)
			if err == nil || !strings.Contains(err.Error(), "exit status 73") {
				t.Fatalf("did not reach SSH after password confirmation: %v", err)
			}
			transcript := clusterInstallRetainedCommandLog(t, out.String())
			if !strings.Contains(transcript, "已完成 0/8") || !strings.Contains(transcript, "exit status 73") || strings.Contains(transcript, secret) {
				t.Fatal("early SSH failure lost its private, credential-free transcript")
			}
			for _, label := range []string{"初始管理员密码", "再次输入管理员密码"} {
				if strings.Count(out.String(), label) != 1 {
					t.Fatalf("expected exactly one %s prompt", label)
				}
			}
			for _, forbidden := range []string{"SSH 配置方式", "保存以上配置", "管理员密码文件", secret} {
				if strings.Contains(out.String(), forbidden) || strings.Contains(err.Error(), secret) {
					t.Fatal("configuration wizard or password content appeared in output")
				}
			}
			calls, err := os.ReadFile(log)
			if err != nil || strings.Count(string(calls), "-G\n") != 3 || strings.Contains(string(calls), secret) {
				t.Fatal("unexpected SSH resolution or password in command arguments")
			}
			after, _ := os.Stat(config)
			raw, _ := os.ReadFile(config)
			if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) || string(raw) != clusterPasswordPromptConfig {
				t.Fatal("saved YAML was modified")
			}
			entries, err := os.ReadDir(filepath.Dir(config))
			wantFiles := 1
			if existing {
				wantFiles++
				raw, err := os.ReadFile(passwordPath)
				if err != nil || string(raw) != "old-password" {
					t.Fatal("existing password file was changed")
				}
			}
			if err != nil || len(entries) != wantFiles {
				t.Fatal("password prompting created a local file")
			}
		})
	}
}

func TestClusterInstallConfigPasswordStopsBeforeSSH(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        error
	}{
		{"EOF", "", io.ErrUnexpectedEOF},
		{"Ctrl-C", "unfinished\x03", context.Canceled},
		{"Ctrl-D", "unfinished\x04", io.ErrUnexpectedEOF},
		{"confirmation EOF", "password\n", io.ErrUnexpectedEOF},
		{"confirmation cancel", "password\n\x03", context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := clusterPasswordPromptSSHFailure(t)
			config := writeClusterConfig(t, clusterPasswordPromptConfig)
			streams, _ := clusterWizardIO()
			streams.In = strings.NewReader(tc.input)
			err := clusterInstallCommandWithIO(t.Context(), []string{"--binary", "/tmp/gpuworks-ha", "--config", config}, streams)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if _, err := os.Stat(log); !os.IsNotExist(err) {
				t.Fatal("SSH ran after password input was interrupted")
			}
			entries, _ := os.ReadDir(filepath.Dir(config))
			if len(entries) != 1 {
				t.Fatal("interrupted input created a password file")
			}
		})
	}
}

func TestClusterInstallConfigPasswordPromptHonorsExplicitInputAndValidation(t *testing.T) {
	for _, name := range []string{"no terminal", "invalid config", "invalid port", "missing file", "unsafe file", "empty explicit file", "valid file", "short form", "canceled"} {
		t.Run(name, func(t *testing.T) {
			log := clusterPasswordPromptSSHFailure(t)
			config := writeClusterConfig(t, clusterPasswordPromptConfig)
			args := []string{"--binary", "/tmp/gpuworks-ha", "--config", config}
			streams, out := clusterWizardIO("unused-password", "unused-password")
			input := streams.In.(*strings.Reader)
			inputSize := input.Len()
			ctx := t.Context()
			switch name {
			case "no terminal":
				streams.IsTerminal = func() bool { return false }
			case "invalid config":
				if err := os.WriteFile(config, []byte("version: 99\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "invalid port":
				args = append(args, "--http-port", "9001")
			case "missing file", "unsafe file", "valid file":
				path := filepath.Join(filepath.Dir(config), "password")
				if name != "missing file" {
					mode := os.FileMode(0600)
					if name == "unsafe file" {
						mode = 0644
					}
					if err := os.WriteFile(path, []byte("explicit-password"), mode); err != nil {
						t.Fatal(err)
					}
				}
				args = append(args, "--password-file", path)
			case "empty explicit file":
				args = append(args, "--password-file=")
			case "short form":
				args = []string{"--binary", "/tmp/gpuworks-ha", "--hosts", "192.0.2.11,192.0.2.12,192.0.2.13", "--ssh-user", "ubuntu"}
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			err := clusterInstallCommandWithIO(ctx, args, streams)
			if err == nil || input.Len() != inputSize || strings.Contains(out.String(), "初始管理员密码") {
				t.Fatal("invalid or explicit settings unexpectedly prompted for a password")
			}
			if name == "no terminal" && (!errors.Is(err, errClusterInstallPasswordRequired) || !strings.Contains(err.Error(), "--password-file")) {
				t.Fatalf("missing actionable unattended error: %v", err)
			}
			_, statErr := os.Stat(log)
			if name == "valid file" {
				if statErr != nil || !strings.Contains(err.Error(), "exit status 73") {
					t.Fatal("explicit password file did not continue to SSH")
				}
			} else if !os.IsNotExist(statErr) {
				t.Fatal("SSH ran before password/config validation completed")
			}
		})
	}
}

func TestClusterInstallPasswordPromptRetriesAndPreservesInput(t *testing.T) {
	secret := "  confirmed password  "
	streams, out := clusterWizardIO("short", "first-password", "different-password", secret, secret)
	password, err := promptClusterInstallPassword(t.Context(), streams)
	if err != nil || password != secret {
		t.Fatalf("confirmed password was not preserved: %v", err)
	}
	if !strings.Contains(out.String(), "密码须为单行") || !strings.Contains(out.String(), "两次密码不一致") || strings.Contains(out.String(), secret) {
		t.Fatal("invalid password handling or password output differs")
	}
}

func clusterInstallRetainedCommandLog(t *testing.T, output string) string {
	t.Helper()
	_, rest, ok := strings.Cut(output, "本地安装日志：")
	if !ok {
		t.Fatal("no installation log path was announced")
	}
	path, _, ok := strings.Cut(rest, "（")
	if !ok || !strings.HasPrefix(path, "/tmp/gpuworks-cluster-install-") || filepath.Base(path) != "install.log" {
		t.Fatal("invalid private installation log path")
	}
	t.Cleanup(func() { _ = os.Remove(path); _ = os.Remove(filepath.Dir(path)) })
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestClusterInstallCommandRetainsTimedOutSSHLog(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nexec /bin/sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	config := writeClusterConfig(t, clusterPasswordPromptConfig)
	password := filepath.Join(dir, "password")
	if err := os.WriteFile(password, []byte("private-password"), 0600); err != nil {
		t.Fatal(err)
	}
	streams, out := clusterWizardIO()
	before := time.Now()
	err := clusterInstallCommandWithIO(t.Context(), []string{"--binary", "/tmp/gpuworks-ha", "--config", config, "--password-file", password, "--timeout", "150ms"}, streams)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(before) > 5*time.Second {
		t.Fatalf("SSH deadline was not preserved: %v", err)
	}
	transcript := clusterInstallRetainedCommandLog(t, out.String())
	if !strings.Contains(transcript, "安装超时") || !strings.Contains(transcript, "解析 SSH 配置") || strings.Contains(transcript, "100%") {
		t.Fatal("deadline transcript lost the unfinished stage")
	}
}
