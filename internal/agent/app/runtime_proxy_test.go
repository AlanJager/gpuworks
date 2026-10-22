package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

type fakeProxyManager struct {
	mu         sync.Mutex
	applied    chan applyCall
	status     *proto.RuntimeProxyStatus
	restartErr error
	restarts   int
	statusCall int
}

type applyCall struct {
	cfg       proto.NetworkConfig
	serverURL string
}

func newFakeProxyManager() *fakeProxyManager {
	return &fakeProxyManager{applied: make(chan applyCall, 8), status: &proto.RuntimeProxyStatus{Privilege: "root", Systemd: true}}
}

func (f *fakeProxyManager) Status(context.Context) *proto.RuntimeProxyStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCall++
	return f.status
}

func (f *fakeProxyManager) Daemon(context.Context) (string, string, string, bool) {
	return "http://d:1", "http://d:1", "", true
}

func (f *fakeProxyManager) Apply(_ context.Context, cfg proto.NetworkConfig, serverURL string) error {
	f.applied <- applyCall{cfg, serverURL}
	return nil
}

func (f *fakeProxyManager) Restart(context.Context) (*proto.RuntimeProxyStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts++
	if f.restartErr != nil {
		return nil, f.restartErr
	}
	return &proto.RuntimeProxyStatus{HTTPProxy: "http://fresh:1", Privilege: "sudo"}, nil
}

type namedPullDriver struct {
	*candidateDriver
	name string
}

func (d namedPullDriver) Name() string { return d.name }

func nodeWithRuntime(t *testing.T, name string, m runtimeProxyManager) *NodeRuntime {
	t.Helper()
	n := NewNodeRuntime(slog.Default(), namedPullDriver{newCandidateDriver(), name}, t.TempDir())
	if m != nil {
		n.runtimeProxy = m
	}
	n.serverURL = "https://gw.example.com:8800"
	return n
}

func TestStateSyncAppliesTheNetworkProxyToTheDockerDaemon(t *testing.T) {
	m := newFakeProxyManager()
	n := nodeWithRuntime(t, "docker", m)
	cfg := proto.NetworkConfig{HTTPProxy: "http://p:3128", HTTPSProxy: "http://p:3128", NoProxy: ".corp"}
	n.OnStateSync(context.Background(), proto.DesiredState{Network: cfg})
	select {
	case got := <-m.applied:
		if got.cfg.HTTPProxy != "http://p:3128" || got.cfg.NoProxy != ".corp" || got.serverURL != "https://gw.example.com:8800" {
			t.Fatalf("apply = %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy was not applied")
	}
	// A cleared proxy is applied too, so the drop-in can be removed.
	n.OnStateSync(context.Background(), proto.DesiredState{})
	select {
	case got := <-m.applied:
		if got.cfg.HTTPProxy != "" {
			t.Fatalf("apply = %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("clearing the proxy was not applied")
	}
}

func TestRuntimeProxyIsOnlyForDocker(t *testing.T) {
	for _, name := range []string{"nerdctl", "udocker", "host"} {
		m := newFakeProxyManager()
		n := nodeWithRuntime(t, name, m)
		n.OnStateSync(context.Background(), proto.DesiredState{Network: proto.NetworkConfig{HTTPProxy: "http://p:3128"}})
		select {
		case got := <-m.applied:
			t.Fatalf("%s: the Docker daemon's proxy was configured: %+v", name, got)
		case <-time.After(50 * time.Millisecond):
		}
		if st := n.RuntimeProxyStatus(context.Background()); st != nil {
			t.Fatalf("%s: reported a daemon proxy: %+v", name, st)
		}
		if _, _, _, ok := n.daemonProxy(context.Background()); ok {
			t.Fatalf("%s: has a daemon proxy", name)
		}
	}
	// No manager at all (not Linux, or a test): docker reports nothing either.
	n := nodeWithRuntime(t, "docker", nil)
	n.OnStateSync(context.Background(), proto.DesiredState{Network: proto.NetworkConfig{HTTPProxy: "http://p:3128"}})
	if st := n.RuntimeProxyStatus(context.Background()); st != nil {
		t.Fatalf("status without a manager: %+v", st)
	}
}

func TestRuntimeProxyStatusComesFromTheManagerForDocker(t *testing.T) {
	m := newFakeProxyManager()
	n := nodeWithRuntime(t, "docker", m)
	st := n.RuntimeProxyStatus(context.Background())
	if st == nil || st.Privilege != "root" || !st.Systemd {
		t.Fatalf("status = %+v", st)
	}
	if h, _, _, ok := n.daemonProxy(context.Background()); !ok || h != "http://d:1" {
		t.Fatalf("daemon proxy = %q %v", h, ok)
	}
}

func TestRuntimeRestartRPC(t *testing.T) {
	ctx := context.Background()
	m := newFakeProxyManager()
	n := nodeWithRuntime(t, "docker", m)
	got, err := n.OnRPC(ctx, proto.RPCRuntimeRestart, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, ok := got.(proto.RuntimeRestartResult)
	if !ok || res.Proxy == nil || res.Proxy.HTTPProxy != "http://fresh:1" || m.restarts != 1 {
		t.Fatalf("result = %#v restarts=%d", got, m.restarts)
	}

	// The manager's refusal reaches the caller with its reason.
	m.restartErr = errors.New("the agent has neither root nor passwordless sudo, so it cannot restart the Docker daemon")
	if _, err := n.OnRPC(ctx, proto.RPCRuntimeRestart, nil); err == nil || !strings.Contains(err.Error(), "neither root nor passwordless sudo") {
		t.Fatalf("err = %v", err)
	}

	for name, tc := range map[string]struct {
		node *NodeRuntime
		want string
	}{
		"not docker":        {nodeWithRuntime(t, "udocker", newFakeProxyManager()), "udocker runtime has no daemon to restart"},
		"no manager":        {nodeWithRuntime(t, "docker", nil), "cannot manage the Docker daemon on this platform"},
		"no runtime at all": {NewNodeRuntime(slog.Default(), nil, t.TempDir(), "host"), ""},
	} {
		_, err := tc.node.OnRPC(ctx, proto.RPCRuntimeRestart, nil)
		if err == nil || (tc.want != "" && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}
