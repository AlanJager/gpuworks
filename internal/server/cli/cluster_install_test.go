package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func offlineInstallOptions(t *testing.T) clusterInstallOptions {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "gpuworks")
	raw := make([]byte, 32)
	copy(raw, []byte{0x7f, 'E', 'L', 'F'})
	if err := os.WriteFile(binary, raw, 0700); err != nil {
		t.Fatal(err)
	}
	return clusterInstallOptions{Hosts: []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"}, User: "operator", Binary: binary, Dir: "/home/operator/cluster", Password: "private-password", HTTP: 8800, Enrollment: 9443, Database: 9001, NoService: true, Timeout: time.Minute}
}

func TestClusterInstallRejectsUnsafeInputsBeforeRemoteWork(t *testing.T) {
	dir := t.TempDir()
	password := filepath.Join(dir, "password")
	if err := os.WriteFile(password, []byte("private-password\n"), 0600); err != nil {
		t.Fatal(err)
	}
	base := []string{"--binary", "/tmp/gpuworks-ha", "--hosts", "192.0.2.1,192.0.2.2,192.0.2.3", "--ssh-user", "operator", "--dir", "/home/operator/cluster", "--password-file", password}
	cases := [][]string{{"--hosts", "192.0.2.1,192.0.2.1,192.0.2.3"}, {"--hosts", "127.0.0.1,192.0.2.2,192.0.2.3"}, {"--hosts", "192.0.2.1,192.0.2.2"}, {"--ssh-user", "operator;touch /tmp/bad"}, {"--dir", "/"}, {"--dir", "/home/operator/../cluster"}, {"--http-port", "9001"}, {"--database-port", "80"}, {"--timeout", "0s"}, {"--password-file", ""}}
	for _, extra := range cases {
		t.Run(strings.Join(extra, " "), func(t *testing.T) {
			if _, err := parseClusterInstall(append(append([]string{}, base...), extra...), io.Discard); err == nil {
				t.Fatal("unsafe options accepted")
			}
		})
	}
	options, err := parseClusterInstall(base, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if options.Password != "private-password" || options.HTTP != 8800 || options.Enrollment != 9443 || options.Database != 9001 || options.NoService {
		t.Fatalf("wrong offline defaults: %#v", options)
	}
	if err := os.Chmod(password, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseClusterInstall(base, io.Discard); err == nil {
		t.Fatal("world-readable password accepted")
	}
}

// The driver fixture validates the literal node protocol and observes actual
// binary bytes. Errors stop the pipeline; no fixture retries a failed business
// request after its status was accepted as healthy.
func TestClusterInstallOfflineDriverRetainsFailuresAndChecksBusiness(t *testing.T) {
	for _, noService := range []bool{false, true} {
		for _, failure := range []string{"", "preflight", "copy", "prepare", "seed", "invite", "join", "serve-joined", "service", "bootstrap", "bootstrap-malformed", "bootstrap-incomplete", "business", "read-setting", "empty-seed", "partial-response", "unsupported-binary"} {
			t.Run(fmt.Sprintf("no-service=%t/%s", noService, failure), func(t *testing.T) {
				o := offlineInstallOptions(t)
				o.NoService = noService
				var events []string
				var output bytes.Buffer
				sentinel := errors.New("injected retained failure")
				shell := func(ctx context.Context, host, command string, input io.Reader) error {
					if strings.Contains(command, "curl") || strings.Contains(command, "wget") || strings.Contains(command, "python") {
						t.Fatal("offline installer used download or Python")
					}
					phase := "preflight"
					if input != nil {
						phase = "copy"
						raw, err := io.ReadAll(input)
						if err != nil || len(raw) != 32 || string(raw[:4]) != "\x7fELF" {
							t.Fatal("copied a different binary")
						}
					}
					if strings.Contains(command, o.Password) {
						t.Fatal("password leaked into shell command")
					}
					events = append(events, phase+":"+host)
					if failure == phase {
						return sentinel
					}
					return nil
				}
				remote := func(ctx context.Context, host string, input io.Reader) ([]byte, error) {
					var req clusterInstallNodeRequest
					if err := json.NewDecoder(input).Decode(&req); err != nil {
						t.Fatal(err)
					}
					if req.Host != host || req.Dir != o.Dir || req.User != o.User || (req.Action != "capabilities" && req.Password != o.Password) || req.HTTP != 8800 || req.Enrollment != 9443 || req.Database != 9001 || req.NoService != o.NoService {
						t.Fatalf("wrong literal remote parameters: %#v", req)
					}
					events = append(events, req.Action+":"+host)
					if failure == req.Action {
						return nil, sentinel
					}
					switch req.Action {
					case "capabilities":
						if req.Password != "" {
							t.Fatal("capability probe received credentials")
						}
						if failure == "unsupported-binary" {
							return []byte(`{"native_dqlite":false,"os":"linux"}`), nil
						}
						return []byte(`{"native_dqlite":true,"os":"linux"}`), nil
					case "prepare":
						if failure == "partial-response" {
							return []byte(`{"prepared":`), nil
						}
					case "status":
						return []byte(`{"status":"Status report\nOverall: ready (business reads verified)\n"}`), nil
					case "bootstrap":
						for _, target := range o.Hosts {
							found := false
							for _, event := range events {
								found = found || event == "service:"+target
							}
							if !found {
								t.Fatal("material bootstrap ran before all installation services")
							}
						}
						if failure == "bootstrap-malformed" {
							return []byte(`{"bootstrap_ready":"true"}`), nil
						}
						if failure == "bootstrap-incomplete" {
							return []byte(`{"bootstrap_ready":false}`), nil
						}
						return []byte(`{"bootstrap_ready":true}`), nil
					case "seed":
						if failure == "empty-seed" {
							return []byte(`{}`), nil
						}
						return []byte(`{"cluster_id":"owned-cluster","ca_fingerprint":"expected-pin"}`), nil
					case "invite":
						return []byte(`{"cluster_id":"owned-cluster","token":"private-invitation"}`), nil
					case "join":
						if !bytes.Contains(req.Invitation, []byte(`"token":"private-invitation"`)) {
							t.Fatal("invitation was dropped")
						}
					case "business", "read-setting":
						var setting struct{ Key, Value string }
						if err := json.Unmarshal(req.Invitation, &setting); err != nil {
							t.Fatal(err)
						}
						if !strings.HasPrefix(setting.Key, "cluster_install_") || setting.Value == "" {
							t.Fatal("business write/read not tied to run nonce")
						}
					}
					return []byte(`{"completed":true}`), nil
				}
				progress, err := newClusterInstallProgressIn(&output, o.NoService, o.Password, t.TempDir(), time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				// The offline driver starts after the public command's precheck.
				progress.StartStage("预检查")
				progress.CompleteStage()
				err = installClusterWithProgress(t.Context(), o, progress, remote, shell, progress)
				err = progress.Finish(err)
				transcript, readErr := os.ReadFile(progress.Path())
				if readErr != nil {
					t.Fatal(readErr)
				}
				for _, secret := range []string{o.Password, "private-invitation"} {
					if bytes.Contains(transcript, []byte(secret)) {
						t.Fatal("local transcript disclosed credentials")
					}
				}
				if !bytes.Contains(transcript, []byte("节点 1/3")) || !bytes.Contains(transcript, []byte("本地安装日志")) {
					t.Fatal("local transcript lost node context or log path")
				}
				if failure == "" {
					total := 8
					if o.NoService {
						total = 7
					}
					if !strings.Contains(output.String(), fmt.Sprintf("已完成 %d/%d (100%%)", total, total)) {
						t.Fatalf("successful installation lost its completed total: %s", output.String())
					}
					if o.NoService && strings.Contains(output.String(), "启用系统服务") {
						t.Fatal("no-service counted a skipped stage")
					}
					if err != nil {
						t.Fatal(err)
					}
					if len(events) != 6+3+3+1+4+2+3+3+6+3 {
						t.Fatalf("missing offline stage: %v", events)
					}
					if o.NoService && !strings.Contains(output.String(), "Preview processes will not restart after reboot") {
						t.Fatal("detached limitation not disclosed")
					}
				} else {
					if err == nil {
						t.Fatal("failed stage reported successful installation")
					}
					if strings.Contains(output.String(), "passed status") || strings.Contains(output.String(), "100%") {
						t.Fatal("failure printed successful cluster")
					}
					for _, event := range events {
						if strings.Contains(event, "cleanup") || strings.Contains(event, "delete") {
							t.Fatal("failed environment was destroyed")
						}
					}
				}
				if strings.Contains(output.String(), o.Password) || strings.Contains(output.String(), "private-invitation") {
					t.Fatal("installation output disclosed secrets")
				}
			})
		}
	}
}

func TestClusterInstallCancellationStopsStatusConvergence(t *testing.T) {
	o := offlineInstallOptions(t)
	ctx, cancel := context.WithCancel(t.Context())
	var checks int
	remote := func(ctx context.Context, host string, input io.Reader) ([]byte, error) {
		var req clusterInstallNodeRequest
		_ = json.NewDecoder(input).Decode(&req)
		switch req.Action {
		case "capabilities":
			return []byte(`{"native_dqlite":true,"os":"linux"}`), nil
		case "seed":
			return []byte(`{"cluster_id":"owned","ca_fingerprint":"expected-pin"}`), nil
		case "bootstrap":
			return []byte(`{"bootstrap_ready":true}`), nil
		case "status":
			checks++
			cancel()
			return nil, errors.New("not ready")
		case "business":
			t.Fatal("business ran after cancellation")
		}
		return []byte(`{}`), nil
	}
	err := installCluster(ctx, o, io.Discard, remote, func(context.Context, string, string, io.Reader) error { return nil })
	if !errors.Is(err, context.Canceled) || checks != 1 {
		t.Fatalf("cancel did not bound startup: %v, checks %d", err, checks)
	}
}

func TestClusterInstallPrivateFileNeverReplacesEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invitation")
	if err := clusterInstallPrivateFile(path, []byte("original")); err != nil {
		t.Fatal(err)
	}
	if err := clusterInstallPrivateFile(path, []byte("replacement")); err == nil {
		t.Fatal("replaced existing evidence")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if info.Mode().Perm() != 0600 || string(raw) != "original" {
		t.Fatal("secret mode or retained original differs")
	}
}

func TestClusterInstallRejectsSymlinkPassword(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/password"
	link := dir + "/link"
	if err := os.WriteFile(path, []byte("private-password"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	_, err := parseClusterInstall([]string{"--binary", "/tmp/gpuworks-ha", "--hosts", "192.0.2.1,192.0.2.2,192.0.2.3", "--ssh-user", "operator", "--dir", "/home/operator/cluster", "--password-file", link}, io.Discard)
	if err == nil {
		t.Fatal("symlink credential accepted")
	}
}

func TestClusterInstallDefaultServicePreflightIsExclusiveAndScoped(t *testing.T) {
	o := offlineInstallOptions(t)
	o.NoService = false
	checks := 0
	stop := errors.New("preflight denied")
	err := installCluster(t.Context(), o, io.Discard, func(context.Context, string, io.Reader) ([]byte, error) {
		t.Fatal("helper ran before preflight")
		return nil, nil
	}, func(ctx context.Context, host, command string, input io.Reader) error {
		checks++
		if !strings.Contains(command, "sudo -n true") || !strings.Contains(command, "test -d /run/systemd/system") || !strings.Contains(command, "sudo -n test ! -e '/etc/systemd/system/"+clusterInstallUnit(o.Dir)+"'") {
			t.Fatalf("non-scoped service preflight: %s", command)
		}
		if input != nil {
			t.Fatal("installation mutated before all preflights completed")
		}
		return stop
	})
	if !errors.Is(err, stop) || checks != 1 {
		t.Fatalf("service prerequisite was ignored: %v", err)
	}
}

func TestClusterInstallFinalStatusDeadlineRetainsObservationCause(t *testing.T) {
	o := offlineInstallOptions(t)
	ctx, cancel := context.WithCancel(t.Context())
	cause := errors.New("selected joined transport policy is missing")
	remote := func(ctx context.Context, host string, input io.Reader) ([]byte, error) {
		var req clusterInstallNodeRequest
		_ = json.NewDecoder(input).Decode(&req)
		switch req.Action {
		case "capabilities":
			return []byte(`{"native_dqlite":true,"os":"linux"}`), nil
		case "seed":
			return []byte(`{"cluster_id":"owned","ca_fingerprint":"expected-pin"}`), nil
		case "bootstrap":
			return []byte(`{"bootstrap_ready":true}`), nil
		case "status":
			cancel()
			return nil, cause
		case "business":
			t.Fatal("business ran after failed status")
		}
		return []byte(`{}`), nil
	}
	err := installCluster(ctx, o, io.Discard, remote, func(context.Context, string, string, io.Reader) error { return nil })
	if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) || !strings.Contains(err.Error(), cause.Error()) {
		t.Fatalf("final status discarded its source cause: %v", err)
	}
}
