// Package sleep is the single home of the server's sleep/wake policy: what a
// sleeping instance still occupies on its GPU, whether a deployment may sleep
// at all, whether a sleeping deployment may be woken without oversubscribing
// its GPUs.
//
// It is a leaf: it never reads the store and knows no engine by name. Engine
// capability arrives as the engine's own deploy.SleepCaps; GPU occupancy
// arrives as snapshots the caller derived from the ledger. The plumbing that
// carries a desired sleep level to the engine (store setter, controller push,
// agent HTTP calls, gateway gating) stays where it is and consults this package
// for every decision.
package sleep

import "gpuworks.ai/gpuworks/internal/server/deploy"

// ResidentBytes is what a sleeping vLLM instance still holds on each of its
// GPUs after it has offloaded (L1) or discarded (L2) its weights and KV cache:
// the CUDA context, allocator pools and the engine's small standing buffers.
//
// It is a per-GPU constant rather than a measurement because the ledger charges
// it before the instance has finished sleeping. Measured residues so far:
// ~4 GB per GPU for DeepSeek-V4-Flash on 4×H200 (2026-08 sleep-mode
// evaluation), 561 MiB (eager) to 995 MiB (CUDA graphs) for a 0.5B model on
// the 2026-09 checkpoint spike host. 1.5 GiB is the agreed middle: charging
// less risks admitting a co-tenant the engine then cannot start, charging more
// wastes memory on every card that hosts a sleeper.
const ResidentBytes int64 = 3 << 29

// Stable error codes for a sleep or wake order the server refuses. They are
// part of the API wire contract; api/errors.go lists the same strings in its
// inventory and asserts they match.
const (
	CodeNotEnabled        = "SLEEP_NOT_ENABLED"
	CodeEngineUnsupported = "SLEEP_ENGINE_UNSUPPORTED"
)

// Deployment is the subset of a deployment's declaration that decides whether
// it may sleep. It is a value type so callers in api, controller and planning
// can build it from their own view of the deployment without sharing a store
// entity.
type Deployment struct {
	Sleepable bool
	Caps      deploy.SleepCaps
}

// Eligible reports whether the deployment may be asked to sleep or wake. The
// returned code is empty when it may; otherwise it is the stable refusal code.
//
// A multi-node replica sleeps like a single-node one: the order goes to its
// leader, whose engine sleeps and wakes the follower ranks with it (the
// followers take no order of their own), as verified on hardware for vLLM
// (ADR 0033).
func Eligible(d Deployment) string {
	if !d.Sleepable {
		return CodeNotEnabled
	}
	if !d.Caps.Supported {
		return CodeEngineUnsupported
	}
	return ""
}

// Declarable reports whether a new deployment may declare itself sleepable.
// It differs from Eligible only in that a deployment that does not ask to be
// sleepable is always fine to create.
func Declarable(d Deployment) string {
	if !d.Sleepable {
		return ""
	}
	return Eligible(d)
}
