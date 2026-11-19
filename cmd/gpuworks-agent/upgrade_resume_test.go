package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/hub"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const upgradeCLIHelperEnv = "GPUWORKS_TEST_UPGRADE_CLI"

// Each copied executable runs the real CLI, including the downloaded smoke
// process and syscall.Exec into run. Normal tests never enter this branch.
func TestMain(m *testing.M) {
	if trace := os.Getenv(upgradeCLIHelperEnv); trace != "" {
		f, err := os.OpenFile(trace, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(90)
		}
		err = json.NewEncoder(f).Encode(os.Args[1:])
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			os.Exit(91)
		}
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type upgradeEndpoint struct {
	server     *httptest.Server
	hub        *hub.Hub
	node       *store.Node
	token      string
	downloads  atomic.Int32
	unexpected atomic.Int32
}

func newUpgradeEndpoint(t *testing.T, binary string) *upgradeEndpoint {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "server.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, token, err := st.CreateNodePending("upgrade-fixture")
	if err != nil {
		t.Fatal(err)
	}
	e := &upgradeEndpoint{hub: hub.New(st, slog.New(slog.NewTextHandler(io.Discard, nil))), node: node, token: token}
	e.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/dl/"+proto.AgentBinaryName(runtime.GOOS, runtime.GOARCH) && r.URL.RawQuery == "":
			e.downloads.Add(1)
			http.ServeFile(w, r, binary)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agent/connect" && (r.URL.RawQuery == "" || r.URL.RawQuery == "lane=ctrl" || r.URL.RawQuery == "lane=data"):
			e.hub.ServeWS(w, r)
		default:
			e.unexpected.Add(1)
			http.Error(w, "unexpected fixture request", http.StatusBadRequest)
		}
	}))
	t.Cleanup(e.server.Close)
	t.Cleanup(e.hub.StopSessions)
	return e
}

func TestManualUpgradePreservesEffectiveLaunchConfiguration(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Reuse the two real servers across this serial matrix; each case drains
	// sessions before another starts, while keeping its own binary and file.
	old := newUpgradeEndpoint(t, binary)
	override := newUpgradeEndpoint(t, binary)
	for _, mode := range []string{"file", "environment", "flags", "flags-over-environment"} {
		t.Run(mode, func(t *testing.T) {
			for _, endpoint := range []*upgradeEndpoint{old, override} {
				endpoint.hub.StartSessions()
				endpoint.downloads.Store(0)
				endpoint.unexpected.Store(0)
				t.Cleanup(endpoint.hub.StopSessions)
			}
			selected := old
			if mode != "file" {
				selected = override
			}
			dir := t.TempDir()
			state := filepath.Join(dir, "state")
			if err := agentconf.Write(state, agentconf.Config{Server: old.server.URL, Token: old.token, RuntimeDriver: "none"}); err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(agentconf.Path(state))
			if err != nil {
				t.Fatal(err)
			}
			// Install a disposable copy: upgrade renames and replaces its own
			// executable, and must never touch the go test runner's binary.
			installed := filepath.Join(dir, "gpuworks-agent")
			copyUpgradeExecutable(t, binary, installed)
			trace := filepath.Join(dir, "argv.jsonl")
			if err := os.WriteFile(trace, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"upgrade", "--state", state}
			env := []string{upgradeCLIHelperEnv + "=" + trace, "GPUWORKS_STATE_DIR=" + state, "GPUWORKS_FAKE_GPU=A10x1", "GPUWORKS_FAKE_RUNTIME_RECORD=" + filepath.Join(dir, "runtime.jsonl")}
			if mode == "environment" {
				env = append(env, "GPUWORKS_SERVER="+selected.server.URL, "GPUWORKS_TOKEN="+selected.token)
			}
			if mode == "flags" || mode == "flags-over-environment" {
				args = append(args, "--server", selected.server.URL, "--token", selected.token)
				if mode == "flags-over-environment" {
					env = append(env, "GPUWORKS_SERVER="+old.server.URL, "GPUWORKS_TOKEN="+old.token)
				}
			}
			cmd := exec.Command(installed, args...)
			// Own the upgrade and any in-flight smoke child together, including
			// on a test failure before the upgrade has reached exec.
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "GPUWORKS_") {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			cmd.Env = append(cmd.Env, env...)
			output, err := os.Create(filepath.Join(dir, "process.log"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = output.Close() })
			cmd.Stdout, cmd.Stderr = output, output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			var waitErr error
			go func() {
				waitErr = cmd.Wait()
				close(done)
			}()
			t.Cleanup(func() {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
				select {
				case <-done:
					if waitErr != nil {
						t.Errorf("owned upgraded CLI exited unsuccessfully: %v", waitErr)
					}
				case <-time.After(10 * time.Second):
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					<-done
					t.Error("owned upgraded CLI did not stop after SIGTERM")
				}
			})
			deadline := time.NewTimer(20 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(20 * time.Millisecond)
			defer tick.Stop()
			for !selected.hub.Online(selected.node.UUID) {
				if selected != old && old.hub.Online(old.node.UUID) {
					t.Fatal("upgrade smoke used the selected credentials, but the exec'd run authenticated with the stale file credentials")
				}
				select {
				case <-done:
					t.Fatalf("CLI exited before the upgraded run authenticated: %v", waitErr)
				case <-deadline.C:
					t.Fatal("upgraded run did not authenticate to the selected server")
				case <-tick.C:
				}
			}
			if selected.downloads.Load() != 1 || (selected != old && old.downloads.Load() != 0) || selected.unexpected.Load() != 0 || old.unexpected.Load() != 0 {
				t.Fatal("upgrade did not use the selected strict download endpoint exclusively")
			}
			unchanged, err := os.ReadFile(agentconf.Path(state))
			if err != nil || !bytes.Equal(original, unchanged) {
				t.Fatal("upgrade changed the on-disk credential configuration")
			}
			assertUpgradeTrace(t, trace, mode == "flags" || mode == "flags-over-environment")
		})
	}
}

func copyUpgradeExecutable(t *testing.T, source, destination string) {
	t.Helper()
	in, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatalf("copy disposable executable: %v, close: %v", copyErr, closeErr)
	}
}

func assertUpgradeTrace(t *testing.T, path string, explicitCredentials bool) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	for _, command := range []string{"upgrade", "smoke", "run"} {
		var args []string
		if err := decoder.Decode(&args); err != nil || len(args) == 0 || args[0] != command {
			t.Fatalf("upgrade lifecycle trace did not reach %s: %v", command, err)
		}
		if command == "run" && !explicitCredentials {
			for _, arg := range args {
				if arg == "--token" || strings.HasPrefix(arg, "--token=") {
					t.Error("file/environment credentials were unnecessarily exposed as run arguments")
				}
			}
		}
	}
	var extra []string
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("unexpected additional CLI lifecycle record: %v", err)
	}
}
