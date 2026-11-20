package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

func clusterWizardIO(lines ...string) (*selfinstall.IO, *bytes.Buffer) {
	out := new(bytes.Buffer)
	streams := &selfinstall.IO{In: strings.NewReader(strings.Join(lines, "\n") + "\n"), Out: out, Err: out, IsTerminal: func() bool { return true }}
	return streams, out
}

func clusterWizardNoExecute(t *testing.T) func(context.Context, []string, bool) error {
	t.Helper()
	return func(context.Context, []string, bool) error {
		t.Error("saving a config must not start a network check or installation")
		return errors.New("unexpected execution")
	}
}

func clusterWizardReadConfig(t *testing.T, path string) clusterInstallOptions {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
	}
	o := clusterInstallOptions{HTTP: 8800, Enrollment: 9443, Database: 9001}
	if err := readClusterInstallConfig(path, &o, nil); err != nil {
		t.Fatalf("generated YAML rejected by real config reader: %v", err)
	}
	if err := validateClusterInstallHosts(&o); err != nil {
		t.Fatal(err)
	}
	if err := validateClusterInstallPorts(o.HTTP, o.Enrollment, o.Database); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := yaml.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{"version": true, "defaults": true, "ports": true, "nodes": true}
	for key := range wire {
		if !keys[key] {
			t.Fatalf("unexpected wire field %q", key)
		}
	}
	if wire["version"] != 1 {
		t.Fatalf("wire version = %v, want 1", wire["version"])
	}
	return o
}

func clusterWizardDefaults() []string {
	return []string{"1", "192.0.2.11", "192.0.2.12", "192.0.2.13", "0"}
}

// ssh -G expands local configuration without attempting remote login.
func clusterWizardSSHFixture(t *testing.T, fails bool) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	realSSH, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "ssh_config")
	if err := os.WriteFile(configPath, []byte("Host *\n  User operator\n  Port 22\n  IdentityFile none\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GPUWORKS_WIZARD_REAL_SSH", realSSH)
	t.Setenv("GPUWORKS_WIZARD_SSH_CONFIG", configPath)
	script := "#!/bin/sh\nlocal_config=no\nfor arg in \"$@\"; do [ \"$arg\" = -G ] && local_config=yes; done\n"
	script += "printf '%s\\n' \"$local_config\" >> \"$GPUWORKS_WIZARD_SSH_CALLS\"\n[ \"$local_config\" = yes ] || exit 99\n"
	if fails {
		script += "exit 1\n"
	} else {
		script += "exec \"$GPUWORKS_WIZARD_REAL_SSH\" -F \"$GPUWORKS_WIZARD_SSH_CONFIG\" \"$@\"\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GPUWORKS_WIZARD_SSH_CALLS", log)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() {
		raw, err := os.ReadFile(log)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		for _, line := range strings.Fields(string(raw)) {
			if line != "yes" {
				t.Fatal("configuration wizard attempted remote SSH")
			}
		}
	})
}

func TestClusterInstallWizardConfigureWritesStrictPrivateConfig(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	lines := append(clusterWizardDefaults(), "y")
	streams, out := clusterWizardIO(lines...)
	err := runClusterInstallWizard(t.Context(), []string{"--configure", "--config", path}, streams, clusterWizardNoExecute(t))
	if err != nil {
		t.Fatalf("configure: %v; output=%s", err, out.String())
	}
	o := clusterWizardReadConfig(t, path)
	if !reflect.DeepEqual(o.Hosts, []string{"192.0.2.11", "192.0.2.12", "192.0.2.13"}) {
		t.Fatalf("hosts = %v", o.Hosts)
	}
	for i, node := range o.Nodes {
		if node.Name != fmt.Sprintf("control-%d", i+1) || node.SSH.Target != node.Address || node.SSH.User != "" || node.SSH.Port != 0 || node.SSH.Key != "" || node.Dir != "/opt/gpuworks/server" {
			t.Fatalf("default node = %+v", node)
		}
	}
	if !strings.Contains(out.String(), "operator") || !strings.Contains(out.String(), "22") || strings.Contains(out.String(), "Admin password") {
		t.Fatalf("expected SSH summary without password prompt: %s", out.String())
	}
}

func TestClusterInstallWizardPerNodeSettingsAndSSHInheritance(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	key := filepath.Join(t.TempDir(), "node.key")
	if err := os.WriteFile(key, []byte("test-only-key"), 0600); err != nil {
		t.Fatal(err)
	}
	lines := []string{"1", "192.0.2.11", "192.0.2.12", "192.0.2.13",
		"1", "alpha-alias", "ubuntu", "2222", key, "/srv/gpuworks-new",
		"2", "beta-alias", "admin", "2203", key, "/home/admin/gpuworks-new",
		"3", "gamma-alias", "operator", "2204", key, "/srv/gpuworks-third",
		"3", "", "-", "-", "-", "", // clear previous explicit SSH settings
		"4", "8802", "9445", "9011", "0", "y"}
	streams, out := clusterWizardIO(lines...)
	if err := runClusterInstallWizard(t.Context(), []string{"--configure", "--config", path}, streams, clusterWizardNoExecute(t)); err != nil {
		t.Fatalf("configure: %v; output=%s", err, out.String())
	}
	o := clusterWizardReadConfig(t, path)
	want := []clusterInstallNode{
		{Name: "control-1", Address: "192.0.2.11", Dir: "/srv/gpuworks-new", SSH: clusterInstallSSH{Target: "alpha-alias", User: "ubuntu", Port: 2222, Key: key}},
		{Name: "control-2", Address: "192.0.2.12", Dir: "/home/admin/gpuworks-new", SSH: clusterInstallSSH{Target: "beta-alias", User: "admin", Port: 2203, Key: key}},
		{Name: "control-3", Address: "192.0.2.13", Dir: "/srv/gpuworks-third", SSH: clusterInstallSSH{Target: "gamma-alias"}},
	}
	if !reflect.DeepEqual(o.Nodes, want) || o.HTTP != 8802 || o.Enrollment != 9445 || o.Database != 9011 {
		t.Fatalf("generated options = %+v, nodes=%+v; want nodes=%+v", o, o.Nodes, want)
	}
	for _, connection := range []string{"ubuntu@alpha-alias:2222", "admin@beta-alias:2203", "operator@gamma-alias:22"} {
		if !strings.Contains(out.String(), connection) {
			t.Fatalf("summary ignored actual OpenSSH precedence for %s: %s", connection, out.String())
		}
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "defaults:") || strings.Contains(string(raw), "password") {
		t.Fatalf("materialized config should omit defaults and passwords: %s", raw)
	}
}

func TestClusterInstallWizardDecliningSaveLeavesNoConfig(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	streams, _ := clusterWizardIO(append(clusterWizardDefaults(), "n")...)
	err := runClusterInstallWizard(t.Context(), []string{"--configure", "--config", path}, streams, clusterWizardNoExecute(t))
	if err != nil {
		t.Fatalf("declining save: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config unexpectedly created: %v", err)
	}
}

func clusterWizardExistingConfig(t *testing.T, path string) []byte {
	t.Helper()
	raw := []byte("version: 1\nports:\n  http: 8802\n  enrollment: 9445\n  database: 9011\nnodes:\n  - name: first\n    address: 192.0.2.21\n    dir: /srv/first-new\n    ssh:\n      target: first-alias\n      user: admin\n      port: 2222\n  - name: second\n    address: 192.0.2.22\n    dir: /srv/second-new\n    ssh:\n      target: second-alias\n      user: operator\n  - name: third\n    address: 192.0.2.23\n    dir: /srv/third-new\n    ssh:\n      target: third-alias\n")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestClusterInstallWizardLoadsExistingAndRequiresOverwriteConsent(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	for _, consent := range []string{"n", "y"} {
		t.Run(consent, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cluster.yaml")
			before := clusterWizardExistingConfig(t, path)
			var previous clusterInstallOptions
			if err := readClusterInstallConfig(path, &previous, nil); err != nil {
				t.Fatal(err)
			}
			lines := append([]string{"1", "", "", "", "0", "y"}, consent)
			streams, out := clusterWizardIO(lines...)
			if err := runClusterInstallWizard(t.Context(), []string{"--configure", "--config", path}, streams, clusterWizardNoExecute(t)); err != nil {
				t.Fatalf("edit: %v; output=%s", err, out.String())
			}
			after, _ := os.ReadFile(path)
			if consent == "n" && !bytes.Equal(before, after) {
				t.Fatal("existing config changed despite declined overwrite")
			}
			o := clusterWizardReadConfig(t, path)
			if !reflect.DeepEqual(o.Nodes, previous.Nodes) || o.HTTP != 8802 || o.Enrollment != 9445 || o.Database != 9011 {
				t.Fatalf("existing configuration not preserved by default answers: %+v", o)
			}
		})
	}
}

func TestClusterInstallWizardRefusesConfigSymlink(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	dir := t.TempDir()
	target, path := filepath.Join(dir, "real.yaml"), filepath.Join(dir, "cluster.yaml")
	before := clusterWizardExistingConfig(t, target)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	streams, _ := clusterWizardIO()
	if err := runClusterInstallWizard(t.Context(), []string{"--configure", "--config", path}, streams, clusterWizardNoExecute(t)); err == nil {
		t.Fatal("symlink configuration accepted")
	}
	after, _ := os.ReadFile(target)
	if !bytes.Equal(before, after) {
		t.Fatal("symlink target was changed")
	}
}

func TestClusterInstallWizardRetriesInvalidInputsAndPortCollisions(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	lines := []string{"1", "127.0.0.1", "::1", "192.0.2.11", "192.0.2.11", "192.0.2.12", "192.0.2.13",
		"bad-choice", "1", "bad@target", "", "invalid user", "-", "65536", "-", "", "../invalid", "",
		"4", "invalid", "8800", "9443", "65535", // derived port overflows
		"8800", "9443", "8799", // derived port collides with HTTP
		"8800", "9443", "9001", "0", "yes"}
	streams, out := clusterWizardIO(lines...)
	if err := runClusterInstallWizard(t.Context(), []string{"--configure", "--config", path}, streams, clusterWizardNoExecute(t)); err != nil {
		t.Fatalf("retry wizard: %v; output=%s", err, out.String())
	}
	o := clusterWizardReadConfig(t, path)
	if o.HTTP != 8800 || o.Enrollment != 9443 || o.Database != 9001 {
		t.Fatalf("ports = %d/%d/%d", o.HTTP, o.Enrollment, o.Database)
	}
	if !strings.Contains(out.String(), "端口配置无效") {
		t.Fatalf("missing actionable retry output: %s", out.String())
	}
}

func TestClusterInstallWizardSaveDefaultAndExecutionCallbacks(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	for _, choice := range []string{"", "2", "3"} {
		t.Run("choice="+choice, func(t *testing.T) {
			dir := t.TempDir()
			path, secretPath := filepath.Join(dir, "cluster.yaml"), filepath.Join(dir, "cluster-admin.password")
			secret := "private-test-password"
			if err := os.WriteFile(secretPath, []byte(secret+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			lines := append(clusterWizardDefaults(), "y", choice)
			if choice == "3" {
				lines = append(lines, "")
			}
			streams, out := clusterWizardIO(lines...)
			calls := 0
			execute := func(ctx context.Context, args []string, check bool) error {
				calls++
				want := []string{"--config", path, "--timeout", "20m0s", "--binary", "/tmp/local-gpuworks", "--no-service"}
				if choice == "3" {
					want = append(want, "--password-file", secretPath)
				}
				if !reflect.DeepEqual(args, want) || check != (choice == "2") {
					t.Fatalf("callback args=%q check=%v, want %q check=%v", args, check, want, choice == "2")
				}
				if ctx.Err() != nil {
					t.Fatal(ctx.Err())
				}
				return nil
			}
			err := runClusterInstallWizard(t.Context(), []string{"--interactive", "--config", path, "--timeout", "20m", "--binary", "/tmp/local-gpuworks", "--no-service"}, streams, execute)
			if err != nil {
				t.Fatalf("wizard: %v; output=%s", err, out.String())
			}
			wantCalls := 1
			if choice == "" {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatalf("callbacks=%d, want %d", calls, wantCalls)
			}
			clusterWizardReadConfig(t, path)
			raw, _ := os.ReadFile(path)
			if strings.Contains(out.String(), secret) || strings.Contains(string(raw), secret) {
				t.Fatal("secret leaked into output or YAML")
			}
			if choice != "3" && strings.Contains(out.String(), "管理员密码文件") {
				t.Fatalf("non-install choice requested credentials: %s", out.String())
			}
		})
	}
}

func TestClusterInstallWizardNoArgumentsDefaultsToSavingOnly(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	dir := t.TempDir()
	t.Chdir(dir)
	streams, _ := clusterWizardIO(append(clusterWizardDefaults(), "y", "")...)
	if err := runClusterInstallWizard(t.Context(), nil, streams, clusterWizardNoExecute(t)); err != nil {
		t.Fatal(err)
	}
	clusterWizardReadConfig(t, filepath.Join(dir, "cluster.yaml"))
}

type clusterWizardHookWriter struct {
	bytes.Buffer
	hook func()
}

func (w *clusterWizardHookWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "配置将保存到") && w.hook != nil {
		hook := w.hook
		w.hook = nil
		hook()
	}
	return w.Buffer.Write(p)
}

func TestClusterInstallWizardPreservesFilesChangedDuringPreview(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	for _, initial := range []string{"existing", "absent"} {
		t.Run(initial, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cluster.yaml")
			lines := append(clusterWizardDefaults(), "y")
			if initial == "existing" {
				clusterWizardExistingConfig(t, path)
				lines = []string{"1", "", "", "", "0", "y", "y"}
			}
			changed := []byte("written independently during preview\n")
			out := &clusterWizardHookWriter{hook: func() {
				if err := os.WriteFile(path, changed, 0600); err != nil {
					t.Error(err)
				}
			}}
			streams, _ := clusterWizardIO(lines...)
			streams.Out, streams.Err = out, out
			if err := runClusterInstallWizard(t.Context(), []string{"--configure", "--config", path}, streams, clusterWizardNoExecute(t)); err == nil {
				t.Fatal("config created/changed during preview was overwritten")
			}
			raw, _ := os.ReadFile(path)
			if !bytes.Equal(raw, changed) {
				t.Fatalf("independent contents overwritten: %s", raw)
			}
			temps, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".cluster-config-*"))
			if err != nil || len(temps) != 0 {
				t.Fatalf("temporary config files left: %v, err=%v", temps, err)
			}
		})
	}
}

type clusterWizardNotifyReader struct {
	io.Reader
	started chan struct{}
	once    sync.Once
}

func (r *clusterWizardNotifyReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	return r.Reader.Read(p)
}

func TestClusterInstallWizardCancellationDuringBlockedPrompt(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	input, writer := io.Pipe()
	defer writer.Close()
	defer input.Close()
	started := make(chan struct{})
	streams := &selfinstall.IO{In: &clusterWizardNotifyReader{Reader: input, started: started}, Out: io.Discard, Err: io.Discard, IsTerminal: func() bool { return true }}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		finished <- runClusterInstallWizard(ctx, []string{"--configure"}, streams, clusterWizardNoExecute(t))
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("wizard did not prompt")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v, want cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wizard blocked after cancellation")
	}
}

func TestClusterInstallWizardCreatesPrivatePasswordWithoutLeaking(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	dir := t.TempDir()
	path, secretPath := filepath.Join(dir, "cluster.yaml"), filepath.Join(dir, "cluster-admin.password")
	secret := "fresh-private-password"
	lines := append(clusterWizardDefaults(), "y", "3", "", secret, secret)
	streams, out := clusterWizardIO(lines...)
	called := false
	err := runClusterInstallWizard(t.Context(), []string{"--interactive", "--config", path, "--binary", "/tmp/gpuworks-ha"}, streams, func(_ context.Context, args []string, check bool) error {
		called = true
		if check || !reflect.DeepEqual(args, []string{"--config", path, "--timeout", "15m0s", "--binary", "/tmp/gpuworks-ha", "--password-file", secretPath}) {
			t.Fatalf("callback args=%q check=%v", args, check)
		}
		var o clusterInstallOptions
		if err := readClusterInstallPassword(secretPath, &o); err != nil || o.Password != secret {
			t.Fatalf("private password invalid: %v", err)
		}
		return nil
	})
	if err != nil || !called {
		t.Fatalf("wizard: %v, called=%v; output=%s", err, called, out.String())
	}
	info, err := os.Stat(secretPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("password mode: %v, info=%v", err, info)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(out.String(), secret) || strings.Contains(string(raw), secret) {
		t.Fatal("password leaked into output or YAML")
	}
}

func TestClusterInstallWizardRejectsUnsafeExistingPassword(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	for _, kind := range []string{"public", "short", "multiline", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path, secretPath := filepath.Join(dir, "cluster.yaml"), filepath.Join(dir, "cluster-admin.password")
			secret, mode := "private-test-password\n", os.FileMode(0600)
			if kind == "public" {
				mode = 0644
			}
			if kind == "short" {
				secret = "s3cr3t!\n"
			}
			if kind == "multiline" {
				secret = "private-test-password\nsecond line\n"
			}
			if err := os.WriteFile(secretPath, []byte(secret), mode); err != nil {
				t.Fatal(err)
			}
			if kind == "symlink" {
				real := filepath.Join(dir, "secret-real")
				if err := os.Rename(secretPath, real); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(real, secretPath); err != nil {
					t.Fatal(err)
				}
			}
			lines := append(clusterWizardDefaults(), "y", "3", "")
			streams, out := clusterWizardIO(lines...)
			if err := runClusterInstallWizard(t.Context(), []string{"--interactive", "--config", path, "--binary", "/tmp/gpuworks-ha"}, streams, clusterWizardNoExecute(t)); err == nil {
				t.Fatal("unsafe password file accepted")
			}
			raw, _ := os.ReadFile(secretPath)
			if string(raw) != secret {
				t.Fatal("existing password file was modified")
			}
			if strings.Contains(out.String(), strings.TrimSpace(secret)) {
				t.Fatal("existing password leaked")
			}
		})
	}
}

func TestClusterInstallWizardPasswordRetriesAndExecutionFailure(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	secret := "accepted-test-password"
	lines := append(clusterWizardDefaults(), "y", "3", "", "tiny", "first-long-password", "different-confirmation", secret, secret)
	streams, out := clusterWizardIO(lines...)
	failure := errors.New("installation fixture failed before remote changes")
	err := runClusterInstallWizard(t.Context(), []string{"--interactive", "--config", path, "--binary", "/tmp/gpuworks-ha"}, streams, func(_ context.Context, args []string, check bool) error {
		if check {
			t.Fatal("requested installation was converted to a check")
		}
		var o clusterInstallOptions
		if err := readClusterInstallPassword(args[len(args)-1], &o); err != nil || o.Password != secret {
			t.Fatalf("retry did not save accepted password: %v", err)
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("callback failure lost: %v", err)
	}
	if !strings.Contains(out.String(), "两次密码不一致") || !strings.Contains(out.String(), "密码须为单行") {
		t.Fatalf("validation errors not reported: %s", out.String())
	}
	for _, candidate := range []string{secret, "first-long-password", "different-confirmation"} {
		if strings.Contains(out.String(), candidate) {
			t.Fatal("password retry leaked a secret")
		}
	}
}

func TestClusterInstallWizardMakesTypedRelativeKeysAbsolute(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	dir := t.TempDir()
	t.Chdir(dir)
	key := filepath.Join(dir, "node.key")
	if err := os.WriteFile(key, []byte("local-test-key"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	lines := []string{"1", "192.0.2.11", "192.0.2.12", "192.0.2.13", "1", "", "", "", "node.key", "", "0"}
	streams, _ := clusterWizardIO(append(lines, "y")...)
	if err := runClusterInstallWizard(t.Context(), []string{"--configure", "--config", path}, streams, clusterWizardNoExecute(t)); err != nil {
		t.Fatal(err)
	}
	for i, node := range clusterWizardReadConfig(t, path).Nodes {
		want := ""
		if i == 0 {
			want = key
		}
		if node.SSH.Key != want {
			t.Fatalf("relative key resolved against config directory: %q, want %q", node.SSH.Key, want)
		}
	}
}

func TestClusterInstallWizardRejectsNonTerminal(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	for _, args := range [][]string{nil, {"--interactive"}, {"--configure"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			streams, out := clusterWizardIO()
			streams.IsTerminal = func() bool { return false }
			err := runClusterInstallWizard(t.Context(), args, streams, clusterWizardNoExecute(t))
			if !errors.Is(err, selfinstall.ErrNoTerminal) {
				t.Fatalf("error = %v, want no-terminal error; output=%s", err, out.String())
			}
			if !strings.Contains(err.Error(), "--") {
				t.Fatalf("no-terminal error needs an actionable flag hint: %v", err)
			}
			clusterWizardAssertNoSSH(t)
		})
	}
}

func TestClusterInstallWizardEOFAndCancelledContext(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	t.Run("EOF", func(t *testing.T) {
		streams, _ := clusterWizardIO()
		streams.In = strings.NewReader("")
		err := runClusterInstallWizard(t.Context(), []string{"--configure", "--config", filepath.Join(t.TempDir(), "cluster.yaml")}, streams, clusterWizardNoExecute(t))
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("error = %v, want unexpected EOF", err)
		}
		clusterWizardAssertNoSSH(t)
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		streams, _ := clusterWizardIO()
		err := runClusterInstallWizard(ctx, []string{"--configure"}, streams, clusterWizardNoExecute(t))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context cancellation", err)
		}
		clusterWizardAssertNoSSH(t)
	})
}
