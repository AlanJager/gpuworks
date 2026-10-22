// kv_cpu_tier.go owns the CPU memory KV tier's size and what the container
// has to give it. vLLM backs the tier with /dev/shm, so its size is both the
// connector's cpu_bytes_to_use and a floor on the container's shared memory:
// both are read from KVCPUTierBytes, which is what keeps them from disagreeing
// (measured: the engine needs the whole tier in /dev/shm, an 800 GiB tier asked
// for 818886 MiB, a 19 GiB tier for 19450 MiB, and it dies at startup with
// "Insufficient space in /dev/shm" when the container has less; see
// artifacts/research/2026-09-22-kv-offload-pd-experiment/PLAN-kv-offload.md and
// docs/reference/deployment-launch-sources.md for the derivation rule).

package deploy

import (
	"fmt"
	"math"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// paramKVOffloadingSize is the dictionary key of the CPU tier's size, in GiB.
const paramKVOffloadingSize = "kv_offloading_size"

// KVCPUTierBytes is the size in bytes of the CPU KV tier a launch
// configuration asks for, or zero when it asks for none. It is the only
// reading of kv_offloading_size: the vLLM connector's cpu_bytes_to_use and the
// container's /dev/shm are both derived from it. The size is the total across
// all workers, not per tensor-parallel rank.
func KVCPUTierBytes(q LaunchConfig) int64 {
	if q.KVOffloadingSize <= 0 {
		return 0
	}
	bytes := q.KVOffloadingSize * float64(uint64(1)<<30)
	if bytes >= math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(bytes)
}

// launchKVCPUTierBytes is the CPU tier the resolved launch will actually run
// with. The configuration's tier counts only when the engine can express it
// and its generated parameters reach the container's command: a complete
// command override (a manual script, or an argv the deployment states in full)
// replaces them, so a tier left in the quick parameters is not in effect there.
func launchKVCPUTierBytes(engine Engine, q LaunchConfig, command EffectiveCommand, overrides ContainerOverrides) int64 {
	if !command.InterpretEngineDialect || overrides.CmdDisplay != "" {
		return 0
	}
	if _, expressible := engineParams(engine.Name())[paramKVOffloadingSize]; !expressible {
		return 0
	}
	return KVCPUTierBytes(q)
}

// launchShmBytes derives the container's /dev/shm size, where zero means the
// launch states none (the container uses the host's /dev/shm, or the runtime's
// own default).
//
//   - --ipc=host: the host's /dev/shm applies and no size is stated; the host's
//     size is not known to the platform, so it is not measured against the tier.
//   - an explicit override wins, but one smaller than the CPU tier can never
//     start, so it is refused here, where preflight and evaluation see it.
//   - otherwise the larger of the engine default plus the CPU tier, and the
//     size the template spec states (templateBytes). The default is kept as
//     headroom for everything else that shares /dev/shm (NCCL, torch IPC): it
//     is fully free when the tier's allocation fails, so the tier is added to
//     it rather than carved out of it. A template's figure is what its engine
//     needs in total, so it is a floor, not an addition. It never reaches the
//     explicit-override branch: a deployer who states a size has chosen it, and
//     only the CPU tier can refuse it.
func launchShmBytes(profileDefault, templateBytes int64, overrides ContainerOverrides, tierBytes int64) (int64, error) {
	if DockerArgsShareHostIPC(overrides.DockerArgs) {
		return 0, nil
	}
	if overrides.ShmBytes > 0 {
		if overrides.ShmBytes < tierBytes {
			return 0, shmBelowKVCPUTier(overrides.ShmBytes, tierBytes)
		}
		return overrides.ShmBytes, nil
	}
	if profileDefault < 0 {
		profileDefault = 0
	}
	derived := profileDefault + tierBytes
	if tierBytes > math.MaxInt64-profileDefault {
		derived = math.MaxInt64
	}
	return max(derived, templateBytes), nil
}

func shmBelowKVCPUTier(shmBytes, tierBytes int64) error {
	// The sizes travel as GiB with two decimals, rounded so the pair never
	// reads as equal: the tier up, the shared memory down.
	shmGiB, tierGiB := gibRoundedDown(shmBytes), gibRoundedUp(tierBytes)
	return messages.NewProductError(messages.ErrorShmBelowKVCPUTier,
		fmt.Sprintf("the shared memory size %g GiB is smaller than the CPU KV tier %g GiB, which the engine keeps in /dev/shm; raise the size to at least the tier or clear it to use the default plus the tier", shmGiB, tierGiB),
		map[string]any{"shm_gib": shmGiB, "tier_gib": tierGiB})
}

func gibRoundedDown(bytes int64) float64 {
	return math.Floor(float64(bytes)/float64(int64(1)<<30)*100) / 100
}

func gibRoundedUp(bytes int64) float64 {
	return math.Ceil(float64(bytes)/float64(int64(1)<<30)*100) / 100
}
