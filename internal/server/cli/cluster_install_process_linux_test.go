//go:build linux

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

// clusterInstallTestChild runs this test executable in one of the roles the
// process tests start it in, and returns only for an ordinary test run.
func clusterInstallTestChild() {
	if len(os.Args) == 2 && os.Args[1] == clusterInstallSupervisorCommand {
		if err := clusterInstallSupervisorCommandRun(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) == 2 && os.Args[1] == "fixture-start" {
		var req clusterInstallNodeRequest
		if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
			os.Exit(62)
		}
		if err := clusterInstallStartContext(context.Background(), req, false, nil); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if os.Getenv("CLUSTER_INSTALL_TEST_CHILD") == "1" {
		// Match the product's signal-aware cleanup, rather than blessing the
		// default OS SIGTERM death as a successful graceful stop.
		terminated := make(chan os.Signal, 1)
		signal.Notify(terminated, syscall.SIGTERM)
		if os.Getenv("CLUSTER_INSTALL_TEST_EXIT") == "default-term" {
			signal.Reset(syscall.SIGTERM)
		}
		_ = syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{})
		ready := filepath.Join(os.Getenv("CLUSTER_INSTALL_TEST_READY_DIR"), fmt.Sprintf("ready-%d", os.Getpid()))
		if err := os.WriteFile(ready, []byte("ready"), 0600); err != nil {
			os.Exit(61)
		}
		<-terminated
		mode := os.Getenv("CLUSTER_INSTALL_TEST_EXIT")
		if mode == "abort" {
			debug.SetTraceback("crash")
			_ = syscall.Kill(os.Getpid(), syscall.SIGABRT)
			select {}
		}
		if mode == "kill" {
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			select {}
		}
		if code, err := strconv.Atoi(mode); err == nil {
			os.Exit(code)
		}
		os.Exit(0)
	}
}

func processInstallFixture(t *testing.T) clusterInstallNodeRequest {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/gpuworks", raw, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir+"/logs", 0700); err != nil {
		t.Fatal(err)
	}
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	req := clusterInstallNodeRequest{Dir: dir, Host: "127.0.0.1", User: account.Username, Password: "private-password"}
	for _, target := range []*int{&req.HTTP, &req.Enrollment, &req.Database} {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		*target = l.Addr().(*net.TCPAddr).Port
		l.Close()
	}
	// Member sessions use the port after the database port, and a start refuses
	// when it is taken: choose a database port whose successor is free as well.
	for attempt := 0; ; attempt++ {
		next, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", req.Database+1))
		if err == nil {
			next.Close()
			break
		}
		if attempt == 50 {
			t.Fatalf("no free database port pair: %v", err)
		}
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		req.Database = l.Addr().(*net.TCPAddr).Port
		l.Close()
	}
	saved := req
	saved.Password = ""
	raw, _ = json.Marshal(saved)
	if err := os.WriteFile(dir+"/cluster-install.json", raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLUSTER_INSTALL_TEST_CHILD", "1")
	t.Setenv("CLUSTER_INSTALL_TEST_READY_DIR", dir+"/logs")
	return req
}

func startInstallFixture(t *testing.T, req clusterInstallNodeRequest, join bool, args []string) error {
	t.Helper()
	if err := clusterInstallStart(req, join, args); err != nil {
		return err
	}
	kind := "serve"
	if join {
		kind = "join"
	}
	pid, err := clusterInstallOwnedProcess(req, kind)
	if err != nil || pid == 0 {
		return fmt.Errorf("fixture child is not owned: %w", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(req.Dir, "logs", fmt.Sprintf("ready-%d", pid))); err == nil {
			return nil
		}
		time.Sleep(time.Millisecond)
	}
	return errors.New("fixture child did not install its signal handler")
}

func TestClusterInstallRejectsNonzeroOwnedExit(t *testing.T) {
	req := processInstallFixture(t)
	t.Setenv("CLUSTER_INSTALL_TEST_EXIT", "7")
	if err := startInstallFixture(t, req, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := clusterInstallStop(t.Context(), req, "serve"); err == nil {
		t.Fatal("owned exit7 was accepted as a graceful transition")
	}
}

func TestClusterInstallRealProcessOwnershipAndStop(t *testing.T) {
	requirePidfd(t)
	req := processInstallFixture(t)
	if err := startInstallFixture(t, req, false, nil); err != nil {
		t.Fatal(err)
	}
	pid, err := clusterInstallOwnedProcess(req, "serve")
	if err != nil || pid <= 1 {
		t.Fatalf("new owned process was not recorded: %d %v", pid, err)
	}
	t.Cleanup(func() {
		if p, e := os.FindProcess(pid); e == nil {
			p.Kill()
		}
	})
	if err := startInstallFixture(t, req, false, nil); err == nil {
		t.Fatal("second process started over live owner")
	}
	if err := clusterInstallStop(t.Context(), req, "serve"); err != nil {
		t.Fatal(err)
	}
	if pid, err := clusterInstallOwnedProcess(req, "serve"); err != nil || pid != 0 {
		t.Fatalf("process did not stop: %d %v", pid, err)
	}
	if err := clusterInstallStop(t.Context(), req, "serve"); err == nil {
		t.Fatal("already absent owner was reported as stopped")
	}
}

func TestClusterInstallRefusesLiveForeignDataAndReusedPID(t *testing.T) {
	req := processInstallFixture(t)
	c := exec.Command(req.Dir+"/gpuworks", "serve", "--data", req.Dir+"/other-data")
	c.Env = append(os.Environ(), "CLUSTER_INSTALL_TEST_CHILD=1")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { c.Process.Kill(); c.Wait() }()
	start, alive, err := clusterInstallProcessIdentity(c.Process.Pid)
	if err != nil || !alive {
		t.Fatal("fixture process did not run")
	}
	raw, _ := json.Marshal(clusterInstallPID{c.Process.Pid, start})
	if err := os.WriteFile(req.Dir+"/serve.pid", raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := clusterInstallStop(t.Context(), req, "serve"); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("foreign data process accepted: %v", err)
	}
	if _, alive, err := clusterInstallProcessIdentity(c.Process.Pid); err != nil || !alive {
		t.Fatal("foreign process was signalled")
	}
	raw, _ = json.Marshal(clusterInstallPID{c.Process.Pid, start + "0"})
	if err := os.WriteFile(req.Dir+"/serve.pid", raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := clusterInstallOwnedProcess(req, "serve"); err == nil || !strings.Contains(err.Error(), "reused") {
		t.Fatalf("PID reuse was not rejected: %v", err)
	}
}

func TestClusterInstallOccupiedPortsKeepExistingListener(t *testing.T) {
	req := processInstallFixture(t)
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", req.HTTP))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := startInstallFixture(t, req, false, nil); err == nil {
		t.Fatal("occupied port accepted")
	}
	if _, err := os.Stat(filepath.Join(req.Dir, "serve.pid")); !os.IsNotExist(err) {
		t.Fatal("occupied port launched process")
	}
}

func TestClusterInstallMissingRecordDoesNotPermitSecondOwner(t *testing.T) {
	req := processInstallFixture(t)
	if err := startInstallFixture(t, req, false, nil); err != nil {
		t.Fatal(err)
	}
	pid, err := clusterInstallOwnedProcess(req, "serve")
	if err != nil || pid == 0 {
		t.Fatal("owned fixture was not started")
	}
	defer func() {
		if p, e := os.FindProcess(pid); e == nil {
			p.Kill()
		}
	}()
	if err := os.Remove(req.Dir + "/serve.pid"); err != nil {
		t.Fatal(err)
	}
	if err := startInstallFixture(t, req, false, nil); err == nil || !strings.Contains(err.Error(), "unrecorded") {
		t.Fatalf("unrecorded live process ignored: %v", err)
	}
}

func TestClusterInstallServiceHandoffUsesScopedUnitAndRetainsDeniedFailure(t *testing.T) {
	requirePidfd(t)
	for _, deny := range []bool{false, true} {
		t.Run(fmt.Sprint(deny), func(t *testing.T) {
			req := processInstallFixture(t)
			if err := startInstallFixture(t, req, false, nil); err != nil {
				t.Fatal(err)
			}
			pid, err := clusterInstallOwnedProcess(req, "serve")
			if err != nil || pid == 0 {
				t.Fatalf("owned service fixture was not started: pid=%d err=%v", pid, err)
			}
			defer func() {
				if p, e := os.FindProcess(pid); e == nil {
					p.Kill()
				}
			}()
			bin := t.TempDir()
			capture := bin + "/unit"
			events := bin + "/events"
			script := `#!/bin/sh
set -eu
test "$1" = -n
shift
printf '%s\n' "$*" >> "$CLUSTER_INSTALL_TEST_EVENTS"
if test "$1" = sh; then
 test "$2" = -c
 case "$3" in *"set -C; cat > '/etc/systemd/system/gpuworks-cluster-"*".service'") ;; *) exit 62 ;; esac
 if test "$CLUSTER_INSTALL_TEST_DENY" = 1; then exit 63; fi
 set -C
 cat > "$CLUSTER_INSTALL_TEST_CAPTURE"
elif test "$1" = systemctl; then
 test -s "$CLUSTER_INSTALL_TEST_CAPTURE"
 case "$2" in daemon-reload) test "$#" = 2 ;; enable) test "$3" = --now; case "$4" in gpuworks-cluster-*.service) ;; *) exit 64 ;; esac ;; *) exit 65 ;; esac
else
 exit 66
fi
`
			if err := os.WriteFile(bin+"/sudo", []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			t.Setenv("CLUSTER_INSTALL_TEST_EVENTS", events)
			t.Setenv("CLUSTER_INSTALL_TEST_CAPTURE", capture)
			if deny {
				t.Setenv("CLUSTER_INSTALL_TEST_DENY", "1")
			} else {
				t.Setenv("CLUSTER_INSTALL_TEST_DENY", "0")
			}
			err = clusterInstallService(t.Context(), req)
			records, _ := os.ReadFile(events)
			if deny {
				if err == nil {
					t.Fatal("privilege refusal reported service success")
				}
				if strings.Contains(string(records), "daemon-reload") || strings.Contains(string(records), "enable") {
					t.Fatal("service activated after failed exclusive publication")
				}
				if _, err := os.Stat(req.Dir + "/serve.pid"); err != nil {
					t.Fatal("denied handoff removed retained process evidence")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				unit, err := os.ReadFile(capture)
				if err != nil {
					t.Fatal(err)
				}
				for _, required := range []string{"User=" + req.User + "\n", "Restart=always\n", "WantedBy=multi-user.target\n", "RequiresMountsFor=" + req.Dir + "/data\n", "ExecStart=" + req.Dir + "/gpuworks serve --config " + req.Dir + "/server.yaml\n"} {
					if !strings.Contains(string(unit), required) {
						t.Fatalf("service omitted %q: %s", required, unit)
					}
				}
				if strings.Contains(string(unit), req.Password) || strings.Contains(string(unit), "GPUWORKS_ADMIN_PASSWORD") {
					t.Fatal("public unit exposed password")
				}
				// The unit names no log target, so its stderr stays on the journal;
				// the configuration it starts from carries the address and log file.
				if strings.Contains(string(unit), "StandardOutput") || strings.Contains(string(unit), "StandardError") {
					t.Fatalf("service redirected its journal output: %s", unit)
				}
				config, err := os.ReadFile(req.Dir + "/server.yaml")
				if err != nil {
					t.Fatal(err)
				}
				wantConfig := "# gpuworks control-plane configuration. Restart the server after editing.\n# docs/reference/configuration.md describes every key.\n" +
					"listen:\n  console:\n    - " + net.JoinHostPort(req.Host, fmt.Sprint(req.HTTP)) + "\n" +
					"data_dir: " + req.Dir + "/data\n" +
					"log:\n  file: " + req.Dir + "/logs/server.log\n  max_size: 10MB\n  keep: 20\n"
				if string(config) != wantConfig || strings.Contains(string(config), req.Password) {
					t.Fatalf("service configuration = %q, want %q", config, wantConfig)
				}
				if info, err := os.Stat(req.Dir + "/server.yaml"); err != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("service configuration is not private: %v", err)
				}
				if !strings.Contains(string(records), "systemctl enable --now "+clusterInstallUnit(req.Dir)) {
					t.Fatalf("scoped restart after reboot was not enabled: %s", records)
				}
			}
		})
	}
}

func TestClusterInstallJoinRequiresFreshOwnedObservationBeyondHistoricalMaterials(t *testing.T) {
	requirePidfd(t)
	for _, dead := range []bool{false, true} {
		t.Run(fmt.Sprint(dead), func(t *testing.T) {
			req := processInstallFixture(t)
			req.ClusterID = "owned-cluster"
			if err := startInstallFixture(t, req, true, []string{"cluster-join", "--data", req.Dir + "/data"}); err != nil {
				t.Fatal(err)
			}
			pid, err := clusterInstallOwnedProcess(req, "join")
			if err != nil || pid == 0 {
				t.Fatal("live joined fixture missing")
			}
			defer func() {
				if p, e := os.FindProcess(pid); e == nil {
					p.Kill()
				}
			}()
			raw := fmt.Sprintf(`{"id":7,"address":%q,"role":"voter","voters":2,"fault_tolerant":false,"materials_ready":%t}`+"\n", fmt.Sprintf("127.0.0.1:%d", req.Database), dead)
			if err := os.WriteFile(req.Dir+"/logs/join.out", []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if dead {
				if err := clusterInstallStop(t.Context(), req, "join"); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			err = clusterInstallWaitJoinObserved(t.Context(), req, func(ctx context.Context) (cluster.QuorumProof, error) {
				calls++
				if calls == 1 {
					return cluster.QuorumProof{}, errors.New("native proof not observed")
				}
				address := fmt.Sprintf("127.0.0.1:%d", req.Database)
				return cluster.QuorumProof{Term: 1, Index: 2, ConfigurationIndex: 1, LeaderID: 8, Members: []cluster.Member{{ID: 7, Address: address, Role: "voter"}, {ID: 8, Address: fmt.Sprintf("192.0.2.1:%d", req.Database), Role: "voter"}}}, nil
			})
			if dead {
				if err == nil || calls != 0 {
					t.Fatal("historically materials-ready log bypassed dead owner")
				}
			} else if err != nil || calls != 2 {
				t.Fatalf("initial materials_ready=false blocked fresh joined observation: %v, calls=%d", err, calls)
			}
		})
	}
}

func TestClusterInstallJoinOwnerMustRemainAliveAcrossFreshObservation(t *testing.T) {
	requirePidfd(t)
	req := processInstallFixture(t)
	req.ClusterID = "owned-cluster"
	if err := startInstallFixture(t, req, true, []string{"cluster-join", "--data", req.Dir + "/data"}); err != nil {
		t.Fatal(err)
	}
	address := fmt.Sprintf("127.0.0.1:%d", req.Database)
	raw := fmt.Sprintf(`{"id":7,"address":%q,"role":"voter","materials_ready":true}`+"\n", address)
	if err := os.WriteFile(req.Dir+"/logs/join.out", []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	err := clusterInstallWaitJoinObserved(t.Context(), req, func(ctx context.Context) (cluster.QuorumProof, error) {
		if err := clusterInstallStop(ctx, req, "join"); err != nil {
			t.Fatal(err)
		}
		return cluster.QuorumProof{Term: 1, Index: 2, ConfigurationIndex: 1, LeaderID: 8, Members: []cluster.Member{{ID: 7, Address: address, Role: "voter"}, {ID: 8, Address: fmt.Sprintf("192.0.2.1:%d", req.Database), Role: "voter"}}}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "exited or changed") {
		t.Fatalf("dead owner accepted from fresh remote leader snapshot: %v", err)
	}
}

func TestClusterInstallJoinDeadlineRetainsActualObservationCause(t *testing.T) {
	req := processInstallFixture(t)
	req.ClusterID = "owned-cluster"
	if err := startInstallFixture(t, req, true, []string{"cluster-join", "--data", req.Dir + "/data"}); err != nil {
		t.Fatal(err)
	}
	pid, err := clusterInstallOwnedProcess(req, "join")
	if err != nil || pid == 0 {
		t.Fatalf("fixture join is not alive: pid=%d err=%v record=%s log=%s", pid, err, readInstallFixtureFile(req.Dir+"/join.pid"), readInstallFixtureFile(req.Dir+"/logs/join.out"))
	}
	defer func() {
		if p, e := os.FindProcess(pid); e == nil {
			p.Kill()
		}
	}()
	address := fmt.Sprintf("127.0.0.1:%d", req.Database)
	raw := fmt.Sprintf(`{"id":7,"address":%q,"role":"spare","voters":1,"materials_ready":false}`+"\n", address)
	if err := os.WriteFile(req.Dir+"/logs/join.out", []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("native barrier response used unsupported schema")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	err = clusterInstallWaitJoinObserved(ctx, req, func(context.Context) (cluster.QuorumProof, error) { return cluster.QuorumProof{}, cause })
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, cause) || !strings.Contains(err.Error(), cause.Error()) {
		t.Fatalf("join timeout discarded observation cause: %v", err)
	}
	if live, err := clusterInstallOwnedProcess(req, "join"); err != nil || live != pid {
		t.Fatal("diagnostic timeout destroyed retained join process")
	}
}

func readInstallFixtureFile(path string) string { raw, _ := os.ReadFile(path); return string(raw) }
