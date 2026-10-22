package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// runtimeProxyManager observes and configures the proxy of the container
// runtime's own daemon (dockerproxy.Manager). Only a Docker daemon has one: the
// other runtimes pull in processes the agent starts.
type runtimeProxyManager interface {
	Status(ctx context.Context) *proto.RuntimeProxyStatus
	Daemon(ctx context.Context) (httpProxy, httpsProxy, noProxy string, ok bool)
	Apply(ctx context.Context, cfg proto.NetworkConfig, serverURL string) error
	Restart(ctx context.Context) (*proto.RuntimeProxyStatus, error)
}

// runtimeProxyApplyTimeout bounds one reconciliation of the daemon's proxy.
const runtimeProxyApplyTimeout = 2 * time.Minute

// dockerProxy is the manager for the active runtime, or nil when that runtime
// is not Docker or the node has no manager (not Linux, or a test).
func (n *NodeRuntime) dockerProxy() runtimeProxyManager {
	if n.runtimeProxy == nil || n.RuntimeDriverName() != "docker" {
		return nil
	}
	return n.runtimeProxy
}

// RuntimeProxyStatus is what the node reports as NodeStatus.RuntimeProxy: nil
// for a runtime that has no daemon proxy.
func (n *NodeRuntime) RuntimeProxyStatus(ctx context.Context) *proto.RuntimeProxyStatus {
	m := n.dockerProxy()
	if m == nil {
		return nil
	}
	return m.Status(ctx)
}

// daemonProxy is the proxy the Docker daemon reports, for ranking sources the
// way the daemon will reach them.
func (n *NodeRuntime) daemonProxy(ctx context.Context) (httpProxy, httpsProxy, noProxy string, ok bool) {
	m := n.dockerProxy()
	if m == nil {
		return "", "", "", false
	}
	return m.Daemon(ctx)
}

// applyRuntimeProxy brings the Docker daemon's proxy drop-in in line with the
// node's network config. It runs in the background, because it may run sudo and
// systemctl, and a failure only shows in the node's status: a state sync is
// never refused over it.
func (n *NodeRuntime) applyRuntimeProxy() {
	m := n.dockerProxy()
	if m == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), runtimeProxyApplyTimeout)
		defer cancel()
		// Read the config when the work starts, so overlapping syncs end on the
		// newest one.
		// The drop-in derives its own NO_PROXY from the configured list
		// (dockerproxy.NoProxyFor), the same one network() applies.
		if err := m.Apply(ctx, n.configuredNetwork(), n.serverURL); err != nil {
			n.log.Warn("docker daemon proxy not applied", "err", err)
		}
	}()
}

// rpcRuntimeRestart restarts the Docker daemon so a changed proxy takes
// effect. The caller chose to; the agent never does it by itself.
func (n *NodeRuntime) rpcRuntimeRestart(ctx context.Context) (any, error) {
	name := n.RuntimeDriverName()
	switch {
	case name == "":
		return nil, errors.New("this node has no container runtime")
	case name != "docker":
		return nil, fmt.Errorf("the %s runtime has no daemon to restart; only Docker does", name)
	case n.runtimeProxy == nil:
		return nil, errors.New("this agent cannot manage the Docker daemon on this platform")
	}
	proxy, err := n.runtimeProxy.Restart(ctx)
	if err != nil {
		return nil, err
	}
	return proto.RuntimeRestartResult{Proxy: proxy}, nil
}
