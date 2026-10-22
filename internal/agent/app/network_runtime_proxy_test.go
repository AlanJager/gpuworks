package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/dockerproxy"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Exercise the public wire handler through the real daemon proxy manager. Only
// external command execution is replaced, and unexpected commands fail.
type networkProxyCommands struct {
	reload     chan struct{}
	unexpected chan string
}

func (r networkProxyCommands) Run(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	switch line {
	case "docker info --format {{json .}}":
		return []byte(`{"HttpProxy":"","HttpsProxy":"","NoProxy":"","LiveRestoreEnabled":false}`), nil
	case "systemctl cat docker.service":
		return []byte("[Unit]"), nil
	case "systemctl show docker.service --property=Environment --value":
		return nil, nil
	case "systemctl daemon-reload":
		r.reload <- struct{}{}
		return nil, nil
	default:
		r.unexpected <- line
		return nil, fmt.Errorf("unexpected command: %s", line)
	}
}

func TestNetworkStateUpdateReconcilesDockerProxyWithoutRestart(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "run/systemd/system"), 0755); err != nil {
		t.Fatal(err)
	}
	manager := dockerproxy.New(nil)
	commands := networkProxyCommands{make(chan struct{}, 8), make(chan string, 8)}
	manager.Root, manager.Run, manager.Euid = root, commands, func() int { return 0 }
	node := nodeWithRuntime(t, "docker", manager)
	modelRoot := node.ModelsRoot()
	dropin := filepath.Join(root, "etc/systemd/system/docker.service.d/zz-gpuworks-proxy.conf")
	for _, wire := range []string{`{"network":{"http_proxy":"http://replacement.invalid:8080","no_proxy":".corp"}}`, `{"network":{}}`} {
		var update proto.StateUpdate
		if err := json.Unmarshal([]byte(wire), &update); err != nil {
			t.Fatal(err)
		}
		if node.OnStateUpdate(context.Background(), update) {
			t.Fatal("network update refreshed image inventory")
		}
		select {
		case <-commands.reload:
		case line := <-commands.unexpected:
			t.Fatalf("unexpected external effect: %s", line)
		case <-time.After(3 * time.Second):
			t.Fatal("network update did not reconcile the daemon proxy")
		}
		if node.ModelsRoot() != modelRoot {
			t.Fatal("network update changed the models root")
		}
		data, err := os.ReadFile(dropin)
		if update.Network.HTTPProxy == "" {
			if !os.IsNotExist(err) {
				t.Fatalf("cleared proxy drop-in: data=%s err=%v", data, err)
			}
		} else if err != nil || !strings.Contains(string(data), "HTTP_PROXY=http://replacement.invalid:8080") || !strings.Contains(string(data), "gw.example.com") {
			t.Fatalf("daemon proxy drop-in = %s, err=%v", data, err)
		}
	}
	node.OnStateUpdate(context.Background(), proto.StateUpdate{})
	select {
	case line := <-commands.unexpected:
		t.Fatalf("unexpected external effect: %s", line)
	case <-commands.reload:
		t.Fatal("absent network changed the daemon proxy")
	case <-time.After(25 * time.Millisecond):
	}
}
