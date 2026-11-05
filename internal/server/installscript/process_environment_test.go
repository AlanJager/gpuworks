//go:build multiproc

package installscript

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/libp2p/go-yamux/v5"
	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestScriptSelectedConnectionSurvivesInheritedEnvironment(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "gpuworks-agent.upload")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/gpuworks-agent")
	build.Dir = "../../.."
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build agent: %v\n%s", err, out)
	}
	for _, operation := range []string{"reinstall", "restart"} {
		t.Run(operation, func(t *testing.T) {
			callback := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				select {
				case callback <- struct{}{}:
				default:
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer server.Close()
			dir := filepath.Join(t.TempDir(), "agent")
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := listener.Addr().String()
			_ = listener.Close()
			env := append(os.Environ(), "GPUWORKS_AGENT_BINARY="+binary, "GPUWORKS_FAKE_GPU=L4x1",
				"GPUWORKS_SERVER=", "GPUWORKS_TOKEN=", "GPUWORKS_CONNECT_MODE=", "GPUWORKS_SERVER_DIAL_LISTEN=")
			stale := append(append([]string{}, env...), "GPUWORKS_SERVER="+server.URL,
				"GPUWORKS_TOKEN=old-node-token", "GPUWORKS_CONNECT_MODE=agent_dial", "GPUWORKS_SERVER_DIAL_LISTEN=127.0.0.1:1")
			pids := []int{}
			t.Cleanup(func() {
				for _, pid := range pids {
					data, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
					args := strings.Split(string(data), "\x00")
					if len(args) >= 4 && args[0] == filepath.Join(dir, "gpuworks-agent") && args[2] == "--state" && args[3] == dir {
						_ = syscall.Kill(pid, syscall.SIGTERM)
					}
				}
			})
			run := func(environment []string, args ...string) {
				t.Helper()
				command := exec.CommandContext(ctx, "sh", append([]string{"-c", Script, "install.sh", "--install-dir", dir, "--no-sudo", "--supervisor", "nohup", "--driver", "none"}, args...)...)
				command.Env = environment
				if out, err := command.CombinedOutput(); err != nil {
					t.Fatalf("install: %v\n%s", err, out)
				}
				pids = append(pids, installedEnvironmentAgentPID(t, dir))
			}
			if operation == "reinstall" {
				run(env, "--server", server.URL, "--token", "old-node-token", "--connect-mode", "agent_dial")
				select {
				case <-callback:
				case <-time.After(5 * time.Second):
					t.Fatal("initial agent never attempted its callback")
				}
				run(stale, "--token", "new-node-token", "--connect-mode", "server_dial", "--listen", address)
			} else {
				run(env, "--token", "new-node-token", "--connect-mode", "server_dial", "--listen", address)
				verifyInstalledServerDialToken(t, dir, address, "new-node-token")
				pid := pids[len(pids)-1]
				_ = syscall.Kill(pid, syscall.SIGTERM)
				wait := time.NewTicker(10 * time.Millisecond)
				defer wait.Stop()
				for {
					if data, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline"); len(data) == 0 {
						break
					}
					select {
					case <-ctx.Done():
						t.Fatal("agent did not stop")
					case <-wait.C:
					}
				}
				command := exec.CommandContext(ctx, "sh", filepath.Join(dir, "agent-restart.sh"))
				command.Env = stale
				if out, err := command.CombinedOutput(); err != nil {
					t.Fatalf("restart: %v\n%s", err, out)
				}
				pids = append(pids, installedEnvironmentAgentPID(t, dir))
			}
			conf, err := agentconf.Load(dir)
			if err != nil || conf.Token != "new-node-token" || conf.Server != "" || conf.ConnectMode != agentconf.ServerDial || conf.Listen != address {
				t.Fatalf("installer did not persist selected connection settings: %v", err)
			}
			verifyInstalledServerDialToken(t, dir, address, "new-node-token")
		})
	}
}

func installedEnvironmentAgentPID(t *testing.T, dir string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "gpuworks-agent.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid < 1 {
		t.Fatal("invalid agent pid")
	}
	return pid
}

func verifyInstalledServerDialToken(t *testing.T, dir, address, token string) {
	t.Helper()
	var raw net.Conn
	var err error
	for deadline := time.Now().Add(5 * time.Second); ; {
		raw, err = net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			log, _ := os.ReadFile(filepath.Join(dir, "agent.log"))
			t.Fatalf("configured server-dial agent never listened: %v\n%s", err, log)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
	var challenge proto.ServerDialChallenge
	if err := json.NewDecoder(bufio.NewReader(raw)).Decode(&challenge); err != nil {
		t.Fatal(err)
	}
	if challenge.NodeUUID != "" {
		t.Fatal("freshly installed agent unexpectedly has a node UUID")
	}
	if err := json.NewEncoder(raw).Encode(proto.ServerDialProof{HMAC: proto.ServerDialHMAC(token, challenge.Nonce)}); err != nil {
		t.Fatal(err)
	}
	session, err := yamux.Server(raw, yamux.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	stream, err := session.AcceptStream()
	if err != nil {
		t.Fatalf("agent rejected the selected token: %v", err)
	}
	defer stream.Close()
	_ = stream.SetReadDeadline(time.Now().Add(3 * time.Second))
	decoder := json.NewDecoder(stream)
	var header proto.StreamHeader
	var envelope proto.Envelope
	var hello proto.Hello
	if err := decoder.Decode(&header); err != nil || header.Kind != proto.StreamCtrl {
		t.Fatalf("control stream: %v", err)
	}
	if err := decoder.Decode(&envelope); err != nil || envelope.T != proto.THello {
		t.Fatalf("agent hello: %v", err)
	}
	if err := json.Unmarshal(envelope.D, &hello); err != nil || hello.NodeToken != token || hello.ConnectMode != agentconf.ServerDial {
		t.Fatalf("running agent did not use the configured connection settings: %v", err)
	}
}
