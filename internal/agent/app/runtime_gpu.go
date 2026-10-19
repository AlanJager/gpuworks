package app

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// Whether the container runtime this node uses can actually give a
// container its accelerators.
//
// A node can have perfectly good cards and no way to hand them to a
// container: the vendor's container plugin is not installed, or the daemon
// it is installed for was never configured to use it. Every deployment on
// such a node then fails at container start, with an error from the
// runtime that names no cause, and the node keeps being chosen because its
// cards look fine — they are fine. Only the agent can see this, so the
// agent says it.
//
// Two ways of seeing it, in this order of trust:
//
//   - Ask the daemon. Where the runtime has one that can be asked about
//     its GPU support, its answer settles the question.
//   - Look for the vendor's plugin. This is all there is for a runtime
//     with no daemon to ask (nerdctl consumes the toolkit directly), and
//     it is not evidence about a daemon: a toolkit on the agent's PATH
//     does not say what dockerd was configured with.

// runtimeGPUChecker is a container runtime driver that can ask its daemon
// whether NVIDIA GPUs can be given to a container (the docker driver's
// CheckNVIDIAGPU). It is an optional interface so the agent's runtime
// package stays free to add others without this file knowing them.
type runtimeGPUChecker interface {
	CheckNVIDIAGPU(context.Context) error
}

// RuntimeGPU is both runtime-plugin facts of one status report: the
// installed plugin as a display string ("" when none is installed — which
// plugin a node needs is the server's to name, accelerator.RuntimeGPUPlugin),
// and the code saying why containers cannot be given this node's
// accelerators ("" when they can). They come from one detection because
// detecting shells out to the vendor's tooling, and on a node whose driver
// is wedged that call is one of the slow ones.
func (n *NodeRuntime) RuntimeGPU(ctx context.Context) (plugin, blocked string) {
	driver := n.runtimeDriver()
	if driver == nil || n.prober == nil {
		return "", ""
	}
	info := runtime.DetectPlugin(ctx, n.prober, driver.Name())
	switch {
	case !info.Installed:
	case info.Version != "":
		plugin = info.Name + " " + info.Version
	default:
		plugin = info.Name
	}
	return plugin, n.runtimeGPUBlocked(ctx, driver, info)
}

// runtimeGPUVerdict is the last verdict a daemon actually gave. A daemon
// that could not be asked this round — restarting, busy past the timeout —
// has said nothing about the node, and the server cordons on this code: a
// guess in either direction either takes a good node out of service until
// an operator notices, or hands deployments to one that cannot start them.
type runtimeGPUVerdict struct {
	mu   sync.Mutex
	last string
}

func (v *runtimeGPUVerdict) set(blocked string) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.last = blocked
	return blocked
}

func (v *runtimeGPUVerdict) get() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.last
}

// runtimeGPUBlocked is the verdict half of RuntimeGPU.
func (n *NodeRuntime) runtimeGPUBlocked(ctx context.Context, driver runtime.Driver, info runtime.PluginInfo) string {
	blocked, answered := judgeRuntimeGPU(ctx, n.log, n.prober, driver, info)
	if !answered {
		last := n.runtimeGPU.get()
		n.log.Info("container runtime could not be asked about GPU support; keeping the last answer",
			"runtime", driver.Name(), "last", last)
		return last
	}
	return n.runtimeGPU.set(blocked)
}

// judgeRuntimeGPU says why driver cannot give a container the prober's
// accelerators ("" when it can), and answered=false when the runtime's
// daemon could not be asked this time, which says nothing either way. Both
// the status report and runtime auto-detection decide with it, so a
// runtime detection passes over is exactly one the report would cordon.
func judgeRuntimeGPU(ctx context.Context, log *slog.Logger, prober gpu.Prober, driver runtime.Driver, info runtime.PluginInfo) (blocked string, answered bool) {
	vendor := prober.Vendor()
	if !accelerator.RuntimeNeedsGPUPlugin(vendor, driver.Name()) {
		// Either there is no accelerator, or this runtime is handed the
		// device nodes directly. Nothing can be missing.
		return "", true
	}
	// Synthetic cards are injected by no vendor plugin at all, so the
	// host's integration says nothing about whether they can be used.
	if gpu.IsSynthetic(prober) {
		return "", true
	}
	if checker, ok := driver.(runtimeGPUChecker); ok && accelerator.NormalizeVendor(vendor) == accelerator.VendorNVIDIA {
		err := checker.CheckNVIDIAGPU(ctx)
		switch {
		case err == nil:
			// The daemon says it can. A plugin the agent cannot see on its
			// own PATH does not contradict that.
			return "", true
		case errors.Is(err, runtime.ErrNoGPUSupport):
			log.Warn("container runtime cannot use this node's accelerators", "runtime", driver.Name(), "err", err)
			// A toolkit on the host does not say the daemon was configured
			// with it, but no toolkit at all does say what to do first.
			if info.Name != "" && !info.Installed {
				return accelerator.RuntimeGPUBlockedPluginMissing, true
			}
			return accelerator.RuntimeGPUBlockedRuntimeUnconfigured, true
		default:
			log.Info("container runtime could not be asked about GPU support", "runtime", driver.Name(), "err", err)
			return "", false
		}
	}
	if info.Name != "" && !info.Installed {
		return accelerator.RuntimeGPUBlockedPluginMissing, true
	}
	return "", true
}

// KubeUnschedulable is whether this Kubernetes node is cordoned there; false
// on any other runtime, and when the API server cannot be asked (the server
// keeps the node as it was rather than act on a guess).
func (n *NodeRuntime) KubeUnschedulable(ctx context.Context) bool {
	d, ok := n.runtimeDriver().(interface {
		Unschedulable(context.Context) (bool, error)
	})
	if !ok {
		return false
	}
	v, err := d.Unschedulable(ctx)
	if err != nil {
		n.log.Debug("kubernetes node state unavailable", "err", err)
		n.kubeCordon.mu.Lock()
		defer n.kubeCordon.mu.Unlock()
		return n.kubeCordon.last
	}
	n.kubeCordon.mu.Lock()
	n.kubeCordon.last = v
	n.kubeCordon.mu.Unlock()
	return v
}
